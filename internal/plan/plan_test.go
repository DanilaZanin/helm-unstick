package plan

import (
	"strings"
	"testing"

	"github.com/DanilaZanin/helm-unstick/internal/model"
	"github.com/DanilaZanin/helm-unstick/internal/verdict"
)

func stuck(revs ...model.Revision) *model.Stuck {
	return model.Analyze(model.History{Namespace: "prod", Release: "web", Revisions: revs})
}

func rev(n int, s model.Status) model.Revision { return model.Revision{Number: n, Status: s} }

var ctx = Context{Tool: "helm-unstick"}

func lines(p Plan) string {
	var b strings.Builder
	for _, c := range p.Commands {
		b.WriteString(c.Line + "\n")
	}
	return b.String()
}

func TestBuildRollback(t *testing.T) {
	s := stuck(rev(5, model.StatusSuperseded), rev(6, model.StatusDeployed), rev(7, model.StatusPendingUpgrade))
	for _, mode := range []FirstInstall{FirstInstallNone, FirstInstallMarkFailed, FirstInstallUninstall} {
		p := Build(s, mode, ctx)
		if p.Action != ActionRollback || p.RollbackTo != 6 {
			t.Fatalf("mode %q: Action = %s, RollbackTo = %d", mode, p.Action, p.RollbackTo)
		}
	}
	p := Build(s, FirstInstallNone, ctx)
	if !strings.Contains(p.Steps[0], "revision 6") || !strings.Contains(p.Steps[0], "revision 8") {
		t.Errorf("steps do not name the target and the new revision: %v", p.Steps)
	}
	if !strings.Contains(p.Steps[1], "(7)") {
		t.Errorf("steps do not name the pending revision: %v", p.Steps)
	}
	got := lines(p)
	for _, want := range []string{"helm-unstick fix web -n prod\n", "helm rollback web 6 -n prod\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("commands missing %q:\n%s", want, got)
		}
	}
}

func TestBuildRollbackListsLeftoverRevisions(t *testing.T) {
	s := stuck(rev(1, model.StatusDeployed), rev(2, model.StatusPendingUpgrade), rev(3, model.StatusPendingRollback))
	p := Build(s, FirstInstallNone, ctx)
	if !strings.Contains(p.Steps[1], "(2, 3)") {
		t.Errorf("leftover revisions not listed: %v", p.Steps)
	}
}

func TestBuildFirstInstall(t *testing.T) {
	s := stuck(rev(1, model.StatusPendingInstall))
	tests := []struct {
		mode     FirstInstall
		action   Action
		wantLine string
		wantWarn string
	}{
		{FirstInstallNone, ActionChoose, "helm-unstick fix web -n prod --first-install=mark-failed", ""},
		{FirstInstallMarkFailed, ActionMarkFailed, "helm-unstick fix web -n prod --first-install=mark-failed", "already created"},
		{FirstInstallUninstall, ActionUninstall, "helm uninstall web -n prod", "history is deleted"},
	}
	for _, tt := range tests {
		t.Run(string(tt.action), func(t *testing.T) {
			p := Build(s, tt.mode, ctx)
			if p.Action != tt.action {
				t.Fatalf("Action = %s, want %s", p.Action, tt.action)
			}
			if !strings.Contains(lines(p), tt.wantLine) {
				t.Errorf("commands missing %q:\n%s", tt.wantLine, lines(p))
			}
			if tt.wantWarn != "" && !strings.Contains(strings.Join(p.Warnings, "\n"), tt.wantWarn) {
				t.Errorf("warnings missing %q: %v", tt.wantWarn, p.Warnings)
			}
			if p.Summary == "" || len(p.Steps) == 0 {
				t.Errorf("plan is not explained: %+v", p)
			}
		})
	}
}

func TestChoosePlanOffersBothOptionsAndChangesNothing(t *testing.T) {
	p := Build(stuck(rev(1, model.StatusPendingInstall)), FirstInstallNone, ctx)
	got := lines(p)
	if !strings.Contains(got, "--first-install=mark-failed") || !strings.Contains(got, "--first-install=uninstall") {
		t.Errorf("both options must be offered:\n%s", got)
	}
	if !strings.Contains(p.Summary, "first install") {
		t.Errorf("summary: %q", p.Summary)
	}
}

func TestChoosePlanForUpgradeWithoutDeployedRevision(t *testing.T) {
	p := Build(stuck(rev(1, model.StatusFailed), rev(2, model.StatusPendingUpgrade)), FirstInstallNone, ctx)
	if p.Action != ActionChoose || !strings.Contains(p.Summary, "no deployed revision") {
		t.Errorf("Action = %s, summary = %q", p.Action, p.Summary)
	}
}

func TestFirstInstallModeIgnoredWhenRollbackPossible(t *testing.T) {
	s := stuck(rev(1, model.StatusDeployed), rev(2, model.StatusPendingUpgrade))
	if p := Build(s, FirstInstallUninstall, ctx); p.Action != ActionRollback {
		t.Errorf("--first-install must never turn a rollback into an uninstall, got %s", p.Action)
	}
}

func TestCommandsRepeatContextFlags(t *testing.T) {
	c := Context{Tool: "kubectl unstick", Flags: []string{"--older-than 0s", "--kube-context kind-e2e"}}
	p := Build(stuck(rev(1, model.StatusPendingInstall)), FirstInstallNone, c)
	want := "kubectl unstick fix web -n prod --older-than 0s --kube-context kind-e2e --first-install=mark-failed"
	if !strings.Contains(lines(p), want) {
		t.Errorf("commands missing %q:\n%s", want, lines(p))
	}
}

func TestParseFirstInstall(t *testing.T) {
	for in, want := range map[string]FirstInstall{
		"": FirstInstallNone, "uninstall": FirstInstallUninstall, "mark-failed": FirstInstallMarkFailed,
	} {
		got, err := ParseFirstInstall(in)
		if err != nil || got != want {
			t.Errorf("ParseFirstInstall(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"delete", "Uninstall", "mark_failed"} {
		if _, err := ParseFirstInstall(bad); err == nil {
			t.Errorf("ParseFirstInstall(%q) accepted", bad)
		}
	}
}

func TestRecommend(t *testing.T) {
	upgrade := stuck(rev(1, model.StatusDeployed), rev(2, model.StatusPendingUpgrade))
	first := stuck(rev(1, model.StatusPendingInstall))
	tests := []struct {
		name string
		s    *model.Stuck
		v    verdict.Verdict
		want string
	}{
		{"stale upgrade", upgrade, verdict.Stale, "roll back to revision 1"},
		{"stale first install", first, verdict.Stale, "--first-install"},
		{"running", upgrade, verdict.PossiblyRunning, "wait"},
		{"unknown", upgrade, verdict.Unknown, "check access"},
	}
	for _, tt := range tests {
		if got := Recommend(tt.s, tt.v); !strings.Contains(got, tt.want) {
			t.Errorf("%s: Recommend = %q, want it to contain %q", tt.name, got, tt.want)
		}
	}
}

func TestManualCommandsKeepTheClusterSelection(t *testing.T) {
	c := Context{
		Tool:      "helm-unstick",
		Flags:     []string{"--kube-context staging", "--kubeconfig /tmp/kc"},
		HelmFlags: []string{"--kube-context staging", "--kubeconfig /tmp/kc"},
		Driver:    "configmap",
	}
	got := lines(Build(stuck(rev(1, model.StatusDeployed), rev(2, model.StatusPendingUpgrade)), FirstInstallNone, c))
	want := "HELM_DRIVER=configmap helm rollback web 1 -n prod --kube-context staging --kubeconfig /tmp/kc\n"
	if !strings.Contains(got, want) {
		t.Errorf("the manual rollback would run against another cluster or storage:\n%s\nwant line %q", got, want)
	}
	got = lines(Build(stuck(rev(1, model.StatusPendingInstall)), FirstInstallUninstall, c))
	want = "HELM_DRIVER=configmap helm uninstall web -n prod --kube-context staging --kubeconfig /tmp/kc\n"
	if !strings.Contains(got, want) {
		t.Errorf("the manual uninstall would run against another cluster or storage:\n%s\nwant line %q", got, want)
	}
	// the default driver needs no prefix
	got = lines(Build(stuck(rev(1, model.StatusDeployed), rev(2, model.StatusPendingUpgrade)), FirstInstallNone, Context{Tool: "helm-unstick", Driver: "secret"}))
	if strings.Contains(got, "HELM_DRIVER") {
		t.Errorf("secret is Helm's default and needs no HELM_DRIVER:\n%s", got)
	}
}

func TestPlanForSupersededOnlyHistory(t *testing.T) {
	s := stuck(rev(1, model.StatusSuperseded), rev(2, model.StatusSuperseded), rev(3, model.StatusPendingUpgrade))
	p := Build(s, FirstInstallNone, ctx)
	if p.Action != ActionChoose {
		t.Fatalf("Action = %s: an automatic rollback to a superseded revision is a guess and must be explicit", p.Action)
	}
	got := lines(p)
	if !strings.Contains(got, "helm-unstick fix web -n prod --to-revision 2\n") {
		t.Errorf("the plan must offer --to-revision 2:\n%s", got)
	}
	if !strings.Contains(p.Summary, "superseded") {
		t.Errorf("summary does not explain the interrupted write: %q", p.Summary)
	}
	warned := strings.Join(p.Warnings, "\n")
	if !strings.Contains(warned, "2 earlier revisions") || !strings.Contains(warned, "uninstall") {
		t.Errorf("uninstall must be flagged as destructive for a release with history:\n%s", warned)
	}
	// the explicit choice is honored
	p = Build(s, FirstInstallNone, Context{Tool: "helm-unstick", ToRevision: 2})
	if p.Action != ActionRollback || p.RollbackTo != 2 {
		t.Errorf("--to-revision 2: Action = %s, RollbackTo = %d", p.Action, p.RollbackTo)
	}
	if !strings.Contains(lines(p), "--to-revision 2") {
		t.Errorf("the printed fix command must repeat --to-revision:\n%s", lines(p))
	}
}

func TestUninstallIsNotAnEqualOptionForLongHistory(t *testing.T) {
	s := stuck(rev(1, model.StatusFailed), rev(2, model.StatusFailed), rev(3, model.StatusPendingUpgrade))
	p := Build(s, FirstInstallNone, ctx)
	if !strings.Contains(strings.Join(p.Warnings, "\n"), "2 earlier revisions") {
		t.Errorf("no warning about deleting the history:\n%v", p.Warnings)
	}
	first := Build(stuck(rev(1, model.StatusPendingInstall)), FirstInstallNone, ctx)
	if len(first.Warnings) != 0 {
		t.Errorf("a first install has no history to lose: %v", first.Warnings)
	}
}
