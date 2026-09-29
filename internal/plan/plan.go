// Package plan turns an analyzed stuck release into a recovery plan: what will be done,
// what it costs, and the exact commands that do it.
package plan

import (
	"fmt"
	"strings"

	"github.com/DanilaZanin/helm-unstick/internal/model"
	"github.com/DanilaZanin/helm-unstick/internal/verdict"
)

// FirstInstall is the user's choice for a release that has no deployed revision to return to.
type FirstInstall string

// Values of --first-install.
const (
	FirstInstallNone       FirstInstall = ""
	FirstInstallUninstall  FirstInstall = "uninstall"
	FirstInstallMarkFailed FirstInstall = "mark-failed"
)

// ParseFirstInstall validates the --first-install flag value.
func ParseFirstInstall(s string) (FirstInstall, error) {
	switch FirstInstall(s) {
	case FirstInstallNone, FirstInstallUninstall, FirstInstallMarkFailed:
		return FirstInstall(s), nil
	}
	return FirstInstallNone, fmt.Errorf("invalid --first-install value %q: use uninstall or mark-failed", s)
}

// Action is what the plan does to the release.
type Action string

// Plan actions.
const (
	ActionRollback   Action = "rollback"
	ActionUninstall  Action = "uninstall"
	ActionMarkFailed Action = "mark-failed"
	// ActionChoose means the user has to pick: nothing will be changed until they do.
	ActionChoose Action = "choose"
)

// Command is a command line the user can run.
type Command struct {
	Note string
	Line string
}

// Plan is the recovery plan for one stuck release.
type Plan struct {
	Action     Action
	RollbackTo int // revision to roll back to, for ActionRollback
	Summary    string
	Steps      []string
	Warnings   []string
	Commands   []Command
}

// Context carries how the tool was invoked, so printed commands can be pasted as they are.
type Context struct {
	Tool  string   // command prefix: helm-unstick, helm unstick or kubectl unstick
	Flags []string // extra flags to repeat, for example "--older-than 15m"
}

func (c Context) fixCommand(s *model.Stuck, extra ...string) string {
	parts := []string{c.Tool, "fix", s.Release, "-n", s.Namespace}
	parts = append(parts, c.Flags...)
	parts = append(parts, extra...)
	return strings.Join(parts, " ")
}

// Build creates the plan. mode only matters when the release has no rollback target.
func Build(s *model.Stuck, mode FirstInstall, ctx Context) Plan {
	if s.HasRollbackTarget() {
		return rollbackPlan(s, ctx)
	}
	switch mode {
	case FirstInstallMarkFailed:
		return markFailedPlan(s, ctx)
	case FirstInstallUninstall:
		return uninstallPlan(s, ctx)
	}
	return choosePlan(s, ctx)
}

func pendingNumbers(s *model.Stuck) string {
	nums := make([]string, 0, len(s.Leftover)+1)
	for _, r := range s.Leftover {
		nums = append(nums, fmt.Sprint(r.Number))
	}
	nums = append(nums, fmt.Sprint(s.Pending.Number))
	return strings.Join(nums, ", ")
}

func rollbackPlan(s *model.Stuck, ctx Context) Plan {
	p := Plan{Action: ActionRollback, RollbackTo: s.Target.Number}
	p.Summary = fmt.Sprintf("Roll back to revision %d, the newest deployed revision.", s.Target.Number)
	p.Steps = []string{
		fmt.Sprintf("Roll back release %q to revision %d. Helm records the result as revision %d.", s.Release, s.Target.Number, s.NextRevision()),
		fmt.Sprintf("Mark any of the pending revisions (%s) that the rollback leaves behind as failed, so the history stops showing an operation in progress.", pendingNumbers(s)),
	}
	p.Commands = []Command{
		{Note: "recover with helm-unstick", Line: ctx.fixCommand(s)},
		{Note: "the rollback step alone, by hand", Line: fmt.Sprintf("helm rollback %s %d -n %s", s.Release, s.Target.Number, s.Namespace)},
	}
	return p
}

func markFailedPlan(s *model.Stuck, ctx Context) Plan {
	p := Plan{Action: ActionMarkFailed}
	p.Summary = fmt.Sprintf("Mark revision %d as failed so the next helm upgrade --install can proceed.", s.Pending.Number)
	p.Steps = []string{
		fmt.Sprintf("Set the status of revision %d to failed. No resource is deleted and no other revision changes.", s.Pending.Number),
		"Run your normal helm upgrade --install afterwards.",
	}
	p.Warnings = []string{
		"Resources the interrupted operation already created stay in the cluster. The next upgrade takes them over.",
	}
	p.Commands = []Command{{Note: "recover with helm-unstick", Line: ctx.fixCommand(s, "--first-install=mark-failed")}}
	return p
}

func uninstallPlan(s *model.Stuck, ctx Context) Plan {
	p := Plan{Action: ActionUninstall}
	p.Summary = fmt.Sprintf("Uninstall release %q and delete its stored history.", s.Release)
	p.Steps = []string{
		fmt.Sprintf("Uninstall release %q: delete the resources listed in the manifest of revision %d and remove every stored revision of the release.", s.Release, s.Pending.Number),
		"Run your normal helm install afterwards.",
	}
	p.Warnings = []string{
		"The release history is deleted for good.",
		"Objects annotated helm.sh/resource-policy: keep, and volumes created from StatefulSet volumeClaimTemplates, are not removed.",
	}
	p.Commands = []Command{
		{Note: "recover with helm-unstick", Line: ctx.fixCommand(s, "--first-install=uninstall")},
		{Note: "the same by hand", Line: fmt.Sprintf("helm uninstall %s -n %s", s.Release, s.Namespace)},
	}
	return p
}

func choosePlan(s *model.Stuck, ctx Context) Plan {
	p := Plan{Action: ActionChoose}
	if s.Pending.Status == model.StatusPendingInstall {
		p.Summary = fmt.Sprintf("Revision %d is an interrupted first install, so there is nothing to roll back to.", s.Pending.Number)
	} else {
		p.Summary = "The history has no deployed revision, so there is nothing to roll back to."
	}
	p.Summary += " Nothing is changed until you pick one of two ways out."
	p.Steps = []string{
		fmt.Sprintf("mark-failed: set revision %d to failed and keep whatever the operation created. The next helm upgrade --install proceeds.", s.Pending.Number),
		"uninstall: remove the release and everything Helm created for it, then install again from scratch.",
	}
	p.Commands = []Command{
		{Note: "keep the created resources", Line: ctx.fixCommand(s, "--first-install=mark-failed")},
		{Note: "start over", Line: ctx.fixCommand(s, "--first-install=uninstall")},
	}
	return p
}

// Recommend is the one-line advice shown by scan.
func Recommend(s *model.Stuck, v verdict.Verdict) string {
	switch v {
	case verdict.PossiblyRunning:
		return "wait: the operation may still be running"
	case verdict.Unknown:
		return "check access, then see explain"
	}
	if s.HasRollbackTarget() {
		return fmt.Sprintf("fix: roll back to revision %d", s.Target.Number)
	}
	return "fix: choose --first-install=mark-failed or uninstall"
}
