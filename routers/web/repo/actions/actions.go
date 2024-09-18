// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package actions

import (
	"bytes"
	"fmt"
	"net/http"
	"slices"
	"strings"

	actions_model "code.gitea.io/gitea/models/actions"
	"code.gitea.io/gitea/models/db"
	git_model "code.gitea.io/gitea/models/git"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unit"
	"code.gitea.io/gitea/modules/actions"
	"code.gitea.io/gitea/modules/base"
	"code.gitea.io/gitea/modules/container"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/gitrepo"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/optional"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/util"
	shared_user "code.gitea.io/gitea/routers/web/shared/user"
	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/services/convert"

	"github.com/nektos/act/pkg/model"
	"gopkg.in/yaml.v3"
)

const (
	tplListActions base.TplName = "repo/actions/list"
	tplViewActions base.TplName = "repo/actions/view"
)

type Workflow struct {
	Entry  git.TreeEntry
	Global bool
	ErrMsg string
}

// MustEnableActions check if actions are enabled in settings
func MustEnableActions(ctx *context.Context) {
	if !setting.Actions.Enabled {
		ctx.NotFound("MustEnableActions", nil)
		return
	}

	if unit.TypeActions.UnitGlobalDisabled() {
		ctx.NotFound("MustEnableActions", nil)
		return
	}

	if ctx.Repo.Repository != nil {
		if !ctx.Repo.CanRead(unit.TypeActions) {
			ctx.NotFound("MustEnableActions", nil)
			return
		}
	}
}

func List(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("actions.actions")
	ctx.Data["PageIsActions"] = true
	workflowID := ctx.FormString("workflow")
	actorID := ctx.FormInt64("actor")
	status := ctx.FormInt("status")
	ctx.Data["CurWorkflow"] = workflowID

	var workflows []Workflow
	var curWorkflow *model.Workflow
	var globalEntries []*git.TreeEntry
	globalWorkflow, err := db.Find[actions_model.RequireAction](ctx, actions_model.FindRequireActionOptions{
		OrgID: ctx.Repo.Repository.Owner.ID,
	})
	if err != nil {
		ctx.ServerError("Global Workflow DB find fail", err)
		return
	}
	if empty, err := ctx.Repo.GitRepo.IsEmpty(); err != nil {
		ctx.ServerError("IsEmpty", err)
		if len(globalWorkflow) < 1 {
			return
		}
	} else if !empty {
		commit, err := ctx.Repo.GitRepo.GetBranchCommit(ctx.Repo.Repository.DefaultBranch)
		if err != nil {
			ctx.ServerError("GetBranchCommit", err)
			return
		}
		entries, err := actions.ListWorkflows(commit)
		if err != nil {
			ctx.ServerError("ListWorkflows", err)
			return
		}
		for _, gEntry := range globalWorkflow {
			if gEntry.RepoName == ctx.Repo.Repository.Name {
				log.Trace("Same Repo conflict: %s\n", gEntry.RepoName)
				continue
			}
			gRepo, _ := repo_model.GetRepositoryByName(ctx, gEntry.OrgID, gEntry.RepoName)
			gGitRepo, _ := gitrepo.OpenRepository(git.DefaultContext, gRepo)
			// it may be a hack for now..... not sure any better way to do this
			gCommit, _ := gGitRepo.GetBranchCommit(gRepo.DefaultBranch)
			gEntries, _ := actions.ListWorkflows(gCommit)
			for _, entry := range gEntries {
				if gEntry.WorkflowName == entry.Name() {
					globalEntries = append(globalEntries, entry)
					entries = append(entries, entry)
				}
			}
		}

		// Get all runner labels
		runners, err := db.Find[actions_model.ActionRunner](ctx, actions_model.FindRunnerOptions{
			RepoID:        ctx.Repo.Repository.ID,
			IsOnline:      optional.Some(true),
			WithAvailable: true,
		})
		if err != nil {
			ctx.ServerError("FindRunners", err)
			return
		}
		allRunnerLabels := make(container.Set[string])
		for _, r := range runners {
			allRunnerLabels.AddMultiple(r.AgentLabels...)
		}

		workflows = make([]Workflow, 0, len(entries))
		for _, entry := range entries {
			var workflowIsGlobal bool
			workflowIsGlobal = false
			for i := range globalEntries {
				if globalEntries[i] == entry {
					workflowIsGlobal = true
				}
			}
			workflow := Workflow{Entry: *entry, Global: workflowIsGlobal}
			content, err := actions.GetContentFromEntry(entry)
			if err != nil {
				ctx.ServerError("GetContentFromEntry", err)
				return
			}
			wf, err := model.ReadWorkflow(bytes.NewReader(content))
			if err != nil {
				workflow.ErrMsg = ctx.Locale.TrString("actions.runs.invalid_workflow_helper", err.Error())
				workflows = append(workflows, workflow)
				continue
			}
			// The workflow must contain at least one job without "needs". Otherwise, a deadlock will occur and no jobs will be able to run.
			hasJobWithoutNeeds := false
			// Check whether have matching runner and a job without "needs"
			emptyJobsNumber := 0
			for _, j := range wf.Jobs {
				if j == nil {
					emptyJobsNumber++
					continue
				}
				if !hasJobWithoutNeeds && len(j.Needs()) == 0 {
					hasJobWithoutNeeds = true
				}
				runsOnList := j.RunsOn()
				for _, ro := range runsOnList {
					if strings.Contains(ro, "${{") {
						// Skip if it contains expressions.
						// The expressions could be very complex and could not be evaluated here,
						// so just skip it, it's OK since it's just a tooltip message.
						continue
					}
					if !allRunnerLabels.Contains(ro) {
						workflow.ErrMsg = ctx.Locale.TrString("actions.runs.no_matching_online_runner_helper", ro)
						break
					}
				}
				if workflow.ErrMsg != "" {
					break
				}
			}
			if !hasJobWithoutNeeds {
				workflow.ErrMsg = ctx.Locale.TrString("actions.runs.no_job_without_needs")
			}
			if emptyJobsNumber == len(wf.Jobs) {
				workflow.ErrMsg = ctx.Locale.TrString("actions.runs.no_job")
			}
			workflows = append(workflows, workflow)

			if workflow.Entry.Name() == workflowID {
				curWorkflow = wf
			}
		}
	}
	ctx.Data["workflows"] = workflows
	ctx.Data["RepoLink"] = ctx.Repo.Repository.Link()

	page := ctx.FormInt("page")
	if page <= 0 {
		page = 1
	}

	workflow := ctx.FormString("workflow")
	isGlobal := false
	ctx.Data["CurWorkflow"] = workflow

	actionsConfig := ctx.Repo.Repository.MustGetUnit(ctx, unit.TypeActions).ActionsConfig()
	ctx.Data["ActionsConfig"] = actionsConfig

	if strings.HasSuffix(ctx.Repo.Repository.Name, ".workflow") {
		ctx.Data["AllowGlobalWorkflow"] = true
	}

	if len(workflowID) > 0 && ctx.Repo.CanWrite(unit.TypeActions) {
		ctx.Data["AllowDisableOrEnableWorkflow"] = ctx.Repo.IsAdmin()
		isWorkflowDisabled := actionsConfig.IsWorkflowDisabled(workflowID)
		ctx.Data["CurWorkflowDisabled"] = isWorkflowDisabled

		if !isWorkflowDisabled && curWorkflow != nil {
			workflowDispatchConfig := workflowDispatchConfig(curWorkflow)
			if workflowDispatchConfig != nil {
				ctx.Data["WorkflowDispatchConfig"] = workflowDispatchConfig

				branchOpts := git_model.FindBranchOptions{
					RepoID:          ctx.Repo.Repository.ID,
					IsDeletedBranch: optional.Some(false),
					ListOptions: db.ListOptions{
						ListAll: true,
					},
				}
				branches, err := git_model.FindBranchNames(ctx, branchOpts)
				if err != nil {
					ctx.ServerError("FindBranchNames", err)
					return
				}
				// always put default branch on the top if it exists
				if slices.Contains(branches, ctx.Repo.Repository.DefaultBranch) {
					branches = util.SliceRemoveAll(branches, ctx.Repo.Repository.DefaultBranch)
					branches = append([]string{ctx.Repo.Repository.DefaultBranch}, branches...)
				}
				ctx.Data["Branches"] = branches

				tags, err := repo_model.GetTagNamesByRepoID(ctx, ctx.Repo.Repository.ID)
				if err != nil {
					ctx.ServerError("GetTagNamesByRepoID", err)
					return
				}
				ctx.Data["Tags"] = tags
			}
		}
		ctx.Data["CurWorkflowDisabled"] = actionsConfig.IsWorkflowDisabled(workflow)
		ctx.Data["CurGlobalWorkflowEnable"] = actionsConfig.IsGlobalWorkflowEnabled(workflow)
		isGlobal = actionsConfig.IsGlobalWorkflowEnabled(workflow)
	}

	// if status or actor query param is not given to frontend href, (href="/<repoLink>/actions")
	// they will be 0 by default, which indicates get all status or actors
	ctx.Data["CurActor"] = actorID
	ctx.Data["CurStatus"] = status
	if actorID > 0 || status > int(actions_model.StatusUnknown) {
		ctx.Data["IsFiltered"] = true
	}

	opts := actions_model.FindRunOptions{
		ListOptions: db.ListOptions{
			Page:     page,
			PageSize: convert.ToCorrectPageSize(ctx.FormInt("limit")),
		},
		RepoID:        ctx.Repo.Repository.ID,
		WorkflowID:    workflowID,
		TriggerUserID: actorID,
	}

	// if status is not StatusUnknown, it means user has selected a status filter
	if actions_model.Status(status) != actions_model.StatusUnknown {
		opts.Status = []actions_model.Status{actions_model.Status(status)}
	}

	runs, total, err := db.FindAndCount[actions_model.ActionRun](ctx, opts)
	if err != nil {
		ctx.ServerError("FindAndCount", err)
		return
	}

	for _, run := range runs {
		run.Repo = ctx.Repo.Repository
	}

	if err := actions_model.RunList(runs).LoadTriggerUser(ctx); err != nil {
		ctx.ServerError("LoadTriggerUser", err)
		return
	}

	ctx.Data["Runs"] = runs

	actors, err := actions_model.GetActors(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.ServerError("GetActors", err)
		return
	}
	ctx.Data["Actors"] = shared_user.MakeSelfOnTop(ctx.Doer, actors)

	ctx.Data["StatusInfoList"] = actions_model.GetStatusInfoList(ctx)

	pager := context.NewPagination(int(total), opts.PageSize, opts.Page, 5)
	pager.SetDefaultParams(ctx)
	pager.AddParamString("workflow", workflowID)
	pager.AddParamString("actor", fmt.Sprint(actorID))
	pager.AddParamString("status", fmt.Sprint(status))
	if isGlobal {
		pager.AddParamString("global", fmt.Sprint(isGlobal))
	}
	ctx.Data["Page"] = pager
	ctx.Data["HasWorkflowsOrRuns"] = len(workflows) > 0 || len(runs) > 0

	ctx.HTML(http.StatusOK, tplListActions)
}

type WorkflowDispatchInput struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Required    bool     `yaml:"required"`
	Default     string   `yaml:"default"`
	Type        string   `yaml:"type"`
	Options     []string `yaml:"options"`
}

type WorkflowDispatch struct {
	Inputs []WorkflowDispatchInput
}

func workflowDispatchConfig(w *model.Workflow) *WorkflowDispatch {
	switch w.RawOn.Kind {
	case yaml.ScalarNode:
		var val string
		if !decodeNode(w.RawOn, &val) {
			return nil
		}
		if val == "workflow_dispatch" {
			return &WorkflowDispatch{}
		}
	case yaml.SequenceNode:
		var val []string
		if !decodeNode(w.RawOn, &val) {
			return nil
		}
		for _, v := range val {
			if v == "workflow_dispatch" {
				return &WorkflowDispatch{}
			}
		}
	case yaml.MappingNode:
		var val map[string]yaml.Node
		if !decodeNode(w.RawOn, &val) {
			return nil
		}

		workflowDispatchNode, found := val["workflow_dispatch"]
		if !found {
			return nil
		}

		var workflowDispatch WorkflowDispatch
		var workflowDispatchVal map[string]yaml.Node
		if !decodeNode(workflowDispatchNode, &workflowDispatchVal) {
			return &workflowDispatch
		}

		inputsNode, found := workflowDispatchVal["inputs"]
		if !found || inputsNode.Kind != yaml.MappingNode {
			return &workflowDispatch
		}

		i := 0
		for {
			if i+1 >= len(inputsNode.Content) {
				break
			}
			var input WorkflowDispatchInput
			if decodeNode(*inputsNode.Content[i+1], &input) {
				input.Name = inputsNode.Content[i].Value
				workflowDispatch.Inputs = append(workflowDispatch.Inputs, input)
			}
			i += 2
		}
		return &workflowDispatch

	default:
		return nil
	}
	return nil
}

func decodeNode(node yaml.Node, out any) bool {
	if err := node.Decode(out); err != nil {
		log.Warn("Failed to decode node %v into %T: %v", node, out, err)
		return false
	}
	return true
}
