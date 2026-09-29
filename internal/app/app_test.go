package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/DanilaZanin/helm-unstick/internal/model"
	"github.com/DanilaZanin/helm-unstick/internal/verdict"
)

var (
	t0      = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	errBusy = errors.New("another operation (install/upgrade/rollback) is in progress")
)

type key struct{ ns, name string }

// fakeBackend is an in-memory Helm. It refuses to roll back over a pending revision when
// refusePending is set, which is one of the two behaviors the real SDK might have.
type fakeBackend struct {
	ns            string
	releases      map[key][]model.Revision
	evidence      verdict.Evidence
	inspectErr    error
	refusePending bool // Rollback fails while the latest revision is pending
	leavePending  bool // a successful Rollback leaves the old pending revision pending
	failRollback  bool // Rollback always fails
	calls         []string
}

func newFake(revs ...model.Revision) *fakeBackend {
	return &fakeBackend{ns: "prod", releases: map[key][]model.Revision{{"prod", "web"}: revs}}
}

func rev(n int, s model.Status, age time.Duration) model.Revision {
	return model.Revision{Number: n, Status: s, Updated: t0.Add(-age), Chart: "web-1.0.0"}
}

func (f *fakeBackend) Namespace() string { return f.ns }

func (f *fakeBackend) history(ns, name string) (model.History, bool) {
	revs, ok := f.releases[key{ns, name}]
	if !ok {
		return model.History{}, false
	}
	return model.History{Namespace: ns, Release: name, Revisions: append([]model.Revision(nil), revs...)}, true
}

func (f *fakeBackend) ListPending(_ context.Context, ns string) ([]model.History, error) {
	var out []model.History
	for k := range f.releases {
		if ns != "" && k.ns != ns {
			continue
		}
		h, _ := f.history(k.ns, k.name)
		for _, r := range h.Revisions {
			if r.Status.IsPending() {
				out = append(out, h)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Release < out[j].Release })
	return out, nil
}

func (f *fakeBackend) History(_ context.Context, ns, name string) (model.History, error) {
	h, ok := f.history(ns, name)
	if !ok {
		return model.History{}, fmt.Errorf("release %q not found in namespace %q: %w", name, ns, model.ErrNotFound)
	}
	return h, nil
}

func (f *fakeBackend) Inspect(_ context.Context, _ *model.Stuck, _ time.Time, _ time.Duration) (verdict.Evidence, error) {
	return f.evidence, f.inspectErr
}

func (f *fakeBackend) latest(ns, name string) *model.Revision {
	revs := f.releases[key{ns, name}]
	best := 0
	for i := range revs {
		if revs[i].Number > revs[best].Number {
			best = i
		}
	}
	return &revs[best]
}

func (f *fakeBackend) Rollback(_ context.Context, ns, name string, revision int, _ model.ActionOptions) error {
	f.calls = append(f.calls, fmt.Sprintf("rollback %d", revision))
	last := f.latest(ns, name)
	if f.failRollback || (f.refusePending && last.Status.IsPending()) {
		return errBusy
	}
	if !f.leavePending && last.Status.IsPending() {
		last.Status = model.StatusSuperseded
	}
	k := key{ns, name}
	f.releases[k] = append(f.releases[k], model.Revision{Number: last.Number + 1, Status: model.StatusDeployed, Updated: t0, Chart: "web-1.0.0"})
	return nil
}

func (f *fakeBackend) MarkFailed(_ context.Context, ns, name string, revision int, _ string) error {
	f.calls = append(f.calls, fmt.Sprintf("mark-failed %d", revision))
	revs := f.releases[key{ns, name}]
	for i := range revs {
		if revs[i].Number == revision {
			revs[i].Status = model.StatusFailed
			return nil
		}
	}
	return errors.New("no such revision")
}

func (f *fakeBackend) Uninstall(_ context.Context, ns, name string, _ model.ActionOptions) error {
	f.calls = append(f.calls, "uninstall")
	delete(f.releases, key{ns, name})
	return nil
}

// run executes a command line against the fake and returns exit code and output.
func run(t *testing.T, f *fakeBackend, stdin string, interactive bool, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	env := Env{
		Stdin: strings.NewReader(stdin), Stdout: &stdout, Stderr: &stderr,
		Now: func() time.Time { return t0 }, Interactive: interactive, Tool: "helm-unstick", Version: "test",
	}
	code := Run(context.Background(), args, env, func(g Global) (Backend, error) { return f, nil })
	return code, stdout.String(), stderr.String()
}

func stuckUpgrade() *fakeBackend {
	return newFake(rev(1, model.StatusSuperseded, 49*time.Hour), rev(2, model.StatusDeployed, 48*time.Hour), rev(3, model.StatusPendingUpgrade, 2*time.Hour))
}

func assertCalls(t *testing.T, f *fakeBackend, want ...string) {
	t.Helper()
	if len(want) == 0 {
		want = nil
	}
	got := f.calls
	if len(got) == 0 {
		got = nil
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("backend writes = %v, want %v", got, want)
	}
}

func statusOf(f *fakeBackend, n int) model.Status {
	for _, r := range f.releases[key{"prod", "web"}] {
		if r.Number == n {
			return r.Status
		}
	}
	return ""
}

func TestScanFindsStuckRelease(t *testing.T) {
	f := stuckUpgrade()
	code, out, _ := run(t, f, "", false, "scan", "-n", "prod")
	if code != ExitStuck {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitStuck, out)
	}
	fields := strings.Fields(strings.Split(out, "\n")[1])
	want := []string{"prod", "web", "pending-upgrade", "3", "2h", "2", "stale"}
	if !reflect.DeepEqual(fields[:7], want) {
		t.Errorf("row = %v, want prefix %v", fields, want)
	}
	if !strings.Contains(out, "roll back to revision 2") {
		t.Errorf("no recovery hint:\n%s", out)
	}
	assertCalls(t, f)
}

func TestScanUsesContextNamespaceByDefault(t *testing.T) {
	f := stuckUpgrade()
	f.ns = "other"
	code, out, _ := run(t, f, "", false, "scan")
	if code != ExitOK || !strings.Contains(out, "namespace other") {
		t.Errorf("exit = %d\n%s", code, out)
	}
}

func TestScanNothingStuck(t *testing.T) {
	f := newFake(rev(1, model.StatusDeployed, time.Hour))
	code, out, _ := run(t, f, "", false, "scan", "-n", "prod")
	if code != ExitOK || !strings.Contains(out, "No release is stuck") {
		t.Errorf("exit = %d\n%s", code, out)
	}
}

func TestScanAllNamespacesJSON(t *testing.T) {
	f := stuckUpgrade()
	f.releases[key{"dev", "api"}] = []model.Revision{rev(1, model.StatusPendingInstall, 5*time.Minute)}
	f.releases[key{"dev", "ok"}] = []model.Revision{rev(1, model.StatusDeployed, time.Hour)}
	code, out, _ := run(t, f, "", false, "scan", "-A", "-o", "json", "--older-than", "10m")
	if code != ExitStuck {
		t.Fatalf("exit = %d", code)
	}
	var rows []struct {
		Namespace, Release, Status, Verdict string
		LastDeployedRevision                *int
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(rows) != 2 || rows[0].Namespace != "dev" || rows[0].Release != "api" || rows[1].Release != "web" {
		t.Fatalf("rows = %+v (want sorted by namespace, only stuck releases)", rows)
	}
	if rows[0].Verdict != "possibly-running" || rows[0].LastDeployedRevision != nil {
		t.Errorf("young first install = %+v", rows[0])
	}
	if rows[1].Verdict != "stale" || rows[1].LastDeployedRevision == nil || *rows[1].LastDeployedRevision != 2 {
		t.Errorf("old upgrade = %+v", rows[1])
	}
}

func TestScanJSONWhenNothingIsStuck(t *testing.T) {
	code, out, _ := run(t, newFake(rev(1, model.StatusDeployed, time.Hour)), "", false, "scan", "-n", "prod", "-o", "json")
	if code != ExitOK || strings.TrimSpace(out) != "[]" {
		t.Errorf("exit = %d, out = %q", code, out)
	}
}

func TestScanFlagErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"A with n":       {"scan", "-A", "-n", "prod"},
		"bad output":     {"scan", "-o", "yaml"},
		"positional":     {"scan", "web"},
		"bad duration":   {"scan", "--older-than", "soon"},
		"negative":       {"scan", "--older-than", "-5m"},
		"unknown flag":   {"scan", "--nope"},
		"unknown driver": {"scan", "--driver", "sql"},
	} {
		code, _, errOut := run(t, stuckUpgrade(), "", false, args...)
		if code != ExitError || errOut == "" {
			t.Errorf("%s: exit = %d, stderr = %q", name, code, errOut)
		}
	}
}

func TestScanVerdictFollowsEvidence(t *testing.T) {
	f := stuckUpgrade()
	f.evidence = verdict.Evidence{Signals: []verdict.Signal{{Kind: verdict.SignalRollout, Object: "Deployment/web", Detail: "rollout in progress: 0 of 1 updated replicas available"}}}
	_, out, _ := run(t, f, "", false, "scan", "-n", "prod")
	if !strings.Contains(out, "possibly-running") {
		t.Errorf("a rollout in progress must block stale:\n%s", out)
	}

	f = stuckUpgrade()
	f.evidence = verdict.Evidence{Errors: []string{"list deployments: forbidden"}}
	_, out, _ = run(t, f, "", false, "scan", "-n", "prod")
	if !strings.Contains(out, "unknown") {
		t.Errorf("missing permissions must give unknown:\n%s", out)
	}

	f = stuckUpgrade()
	f.inspectErr = errors.New("cluster unreachable")
	_, out, _ = run(t, f, "", false, "scan", "-n", "prod", "-o", "json")
	if !strings.Contains(out, `"verdict": "unknown"`) || !strings.Contains(out, "cluster unreachable") {
		t.Errorf("an Inspect failure must give unknown with the cause:\n%s", out)
	}
}

func TestExplain(t *testing.T) {
	f := stuckUpgrade()
	code, out, _ := run(t, f, "", false, "explain", "web", "-n", "prod", "--older-than", "15m")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	for _, want := range []string{
		"Verdict:   stale", "Roll back to revision 2",
		"helm-unstick fix web -n prod --older-than 15m", "helm rollback web 2 -n prod", "History:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("explain missing %q:\n%s", want, out)
		}
	}
	assertCalls(t, f)
}

func TestExplainNotStuckAndMissing(t *testing.T) {
	f := newFake(rev(1, model.StatusDeployed, time.Hour))
	code, out, _ := run(t, f, "", false, "explain", "web", "-n", "prod")
	if code != ExitOK || !strings.Contains(out, "is not stuck") {
		t.Errorf("exit = %d\n%s", code, out)
	}
	code, _, errOut := run(t, f, "", false, "explain", "nope", "-n", "prod")
	if code != ExitError || !strings.Contains(errOut, "not found") {
		t.Errorf("missing release: exit = %d, stderr = %q", code, errOut)
	}
	code, _, _ = run(t, f, "", false, "explain")
	if code != ExitError {
		t.Errorf("explain without a release: exit = %d", code)
	}
}

func TestFixRollbackDirectWorks(t *testing.T) {
	f := stuckUpgrade()
	code, out, errOut := run(t, f, "", false, "fix", "web", "-n", "prod", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s\n%s", code, out, errOut)
	}
	assertCalls(t, f, "rollback 2")
	if !strings.Contains(out, "Rollback path: direct") || !strings.Contains(out, "deployed as revision 4") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestFixAutoFallsBackToMarkFailed(t *testing.T) {
	f := stuckUpgrade()
	f.refusePending = true
	code, out, errOut := run(t, f, "", false, "fix", "web", "-n", "prod", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s\n%s", code, out, errOut)
	}
	assertCalls(t, f, "rollback 2", "mark-failed 3", "rollback 2")
	if !strings.Contains(out, "mark-failed-first worked") {
		t.Errorf("the output must say which path worked:\n%s", out)
	}
	if statusOf(f, 3) != model.StatusFailed || statusOf(f, 4) != model.StatusDeployed {
		t.Errorf("final statuses: rev3 = %s, rev4 = %s", statusOf(f, 3), statusOf(f, 4))
	}
}

func TestFixDirectStrategyDoesNotFallBack(t *testing.T) {
	f := stuckUpgrade()
	f.refusePending = true
	code, _, errOut := run(t, f, "", false, "fix", "web", "-n", "prod", "--yes", "--strategy", "direct")
	if code != ExitError || !strings.Contains(errOut, "another operation") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
	assertCalls(t, f, "rollback 2")
}

func TestFixMarkFailedStrategyMarksFirst(t *testing.T) {
	f := stuckUpgrade()
	f.refusePending = true
	code, out, _ := run(t, f, "", false, "fix", "web", "-n", "prod", "--yes", "--strategy=mark-failed")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	assertCalls(t, f, "mark-failed 3", "rollback 2")
}

func TestFixAutoDoesNotMarkFailedWhenRollbackFailsForGood(t *testing.T) {
	// A rollback that fails and still fails after mark-failed: two attempts, then an error.
	f := stuckUpgrade()
	f.failRollback = true
	code, _, errOut := run(t, f, "", false, "fix", "web", "-n", "prod", "--yes")
	if code != ExitError || !strings.Contains(errOut, "failed again") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
	assertCalls(t, f, "rollback 2", "mark-failed 3", "rollback 2")
}

func TestFixAutoDoesNotRetryWhenStateChanged(t *testing.T) {
	f := stuckUpgrade()
	f.refusePending = true
	fb := &changingBackend{fakeBackend: f}
	var stdout, stderr bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Now: func() time.Time { return t0 }, Tool: "helm-unstick"}
	code := Run(context.Background(), []string{"fix", "web", "-n", "prod", "--yes"}, env, func(Global) (Backend, error) { return fb, nil })
	if code != ExitError || !strings.Contains(stderr.String(), "was not retried") {
		t.Errorf("exit = %d, stderr = %q", code, stderr.String())
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "mark-failed") {
			t.Errorf("must not mark failed after the state changed: %v", f.calls)
		}
	}
}

// changingBackend appends a new revision as a side effect of the first failed rollback.
type changingBackend struct{ *fakeBackend }

func (c *changingBackend) Rollback(ctx context.Context, ns, name string, revision int, o model.ActionOptions) error {
	err := c.fakeBackend.Rollback(ctx, ns, name, revision, o)
	k := key{ns, name}
	c.releases[k] = append(c.releases[k], model.Revision{Number: 4, Status: model.StatusPendingUpgrade, Updated: t0})
	return err
}

func TestFixTidiesRevisionsLeftPending(t *testing.T) {
	f := stuckUpgrade()
	f.leavePending = true
	code, out, _ := run(t, f, "", false, "fix", "web", "-n", "prod", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	assertCalls(t, f, "rollback 2", "mark-failed 3")
	if statusOf(f, 3) != model.StatusFailed {
		t.Errorf("rev3 = %s, want failed", statusOf(f, 3))
	}
}

func TestFixRefusesWhenPossiblyRunning(t *testing.T) {
	for name, args := range map[string][]string{
		"plain":         {"fix", "web", "-n", "prod", "--yes"},
		"force-unknown": {"fix", "web", "-n", "prod", "--yes", "--force-unknown"},
	} {
		f := stuckUpgrade()
		f.evidence = verdict.Evidence{Signals: []verdict.Signal{{Kind: verdict.SignalJobRunning, Object: "Job/web-migrate", Detail: "hook has not finished: 1 active pod(s)"}}}
		code, out, _ := run(t, f, "", false, args...)
		if code != ExitRefused {
			t.Errorf("%s: exit = %d, want %d\n%s", name, code, ExitRefused, out)
		}
		if !strings.Contains(out, "Job/web-migrate") || !strings.Contains(out, "Refused") {
			t.Errorf("%s: refusal must explain itself:\n%s", name, out)
		}
		assertCalls(t, f)
	}
}

func TestFixYoungRecordIsRefusedEvenWithoutEvidence(t *testing.T) {
	f := newFake(rev(1, model.StatusDeployed, 48*time.Hour), rev(2, model.StatusPendingUpgrade, 30*time.Second))
	code, out, _ := run(t, f, "", false, "fix", "web", "-n", "prod", "--yes")
	if code != ExitRefused || !strings.Contains(out, "less than the 10m threshold") {
		t.Errorf("exit = %d\n%s", code, out)
	}
	assertCalls(t, f)
	code, _, _ = run(t, f, "", false, "fix", "web", "-n", "prod", "--yes", "--older-than", "0s")
	if code != ExitOK {
		t.Errorf("with --older-than 0s the same release is fixable, exit = %d", code)
	}
}

func TestFixUnknownNeedsForceUnknown(t *testing.T) {
	f := stuckUpgrade()
	f.evidence = verdict.Evidence{Errors: []string{"get Deployment/web: forbidden"}}
	code, out, _ := run(t, f, "", false, "fix", "web", "-n", "prod", "--yes")
	if code != ExitRefused || !strings.Contains(out, "--force-unknown") {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	assertCalls(t, f)

	code, out, _ = run(t, f, "", false, "fix", "web", "-n", "prod", "--yes", "--force-unknown")
	if code != ExitOK || !strings.Contains(out, "Warning:") {
		t.Fatalf("forced: exit = %d\n%s", code, out)
	}
	assertCalls(t, f, "rollback 2")
}

func TestFixDryRun(t *testing.T) {
	f := stuckUpgrade()
	code, out, _ := run(t, f, "", false, "fix", "web", "-n", "prod", "--dry-run")
	if code != ExitOK || !strings.Contains(out, "Dry run") || !strings.Contains(out, "Roll back to revision 2") {
		t.Errorf("exit = %d\n%s", code, out)
	}
	assertCalls(t, f)
}

func TestFixDryRunStillRefusesLiveRelease(t *testing.T) {
	f := stuckUpgrade()
	f.evidence = verdict.Evidence{Signals: []verdict.Signal{{Object: "Deployment/web", Detail: "busy"}}}
	code, _, _ := run(t, f, "", false, "fix", "web", "-n", "prod", "--dry-run")
	if code != ExitRefused {
		t.Errorf("dry run must report the same verdict, exit = %d", code)
	}
}

func TestFixConfirmation(t *testing.T) {
	t.Run("non-interactive without --yes", func(t *testing.T) {
		f := stuckUpgrade()
		code, _, errOut := run(t, f, "y\n", false, "fix", "web", "-n", "prod")
		if code != ExitError || !strings.Contains(errOut, "--yes") {
			t.Errorf("exit = %d, stderr = %q", code, errOut)
		}
		assertCalls(t, f)
	})
	t.Run("interactive yes", func(t *testing.T) {
		f := stuckUpgrade()
		code, out, _ := run(t, f, "y\n", true, "fix", "web", "-n", "prod")
		if code != ExitOK || !strings.Contains(out, "Proceed? [y/N]") {
			t.Errorf("exit = %d\n%s", code, out)
		}
		assertCalls(t, f, "rollback 2")
	})
	t.Run("interactive no", func(t *testing.T) {
		f := stuckUpgrade()
		code, out, _ := run(t, f, "n\n", true, "fix", "web", "-n", "prod")
		if code != ExitError || !strings.Contains(out, "Aborted") {
			t.Errorf("exit = %d\n%s", code, out)
		}
		assertCalls(t, f)
	})
	t.Run("interactive empty answer", func(t *testing.T) {
		f := stuckUpgrade()
		if code, _, _ := run(t, f, "", true, "fix", "web", "-n", "prod"); code != ExitError {
			t.Errorf("exit = %d", code)
		}
		assertCalls(t, f)
	})
}

func TestFixFirstInstallWithoutChoicePrintsPlanOnly(t *testing.T) {
	f := newFake(rev(1, model.StatusPendingInstall, 3*time.Hour))
	code, out, _ := run(t, f, "", false, "fix", "web", "-n", "prod", "--yes", "--older-than", "15m")
	if code != ExitStuck {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitStuck, out)
	}
	for _, want := range []string{
		"helm-unstick fix web -n prod --older-than 15m --first-install=mark-failed",
		"helm-unstick fix web -n prod --older-than 15m --first-install=uninstall",
		"Nothing was changed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	assertCalls(t, f)
	if statusOf(f, 1) != model.StatusPendingInstall {
		t.Errorf("release was modified: %s", statusOf(f, 1))
	}
}

func TestFixFirstInstallMarkFailed(t *testing.T) {
	f := newFake(rev(1, model.StatusPendingInstall, 3*time.Hour))
	code, out, errOut := run(t, f, "", false, "fix", "web", "-n", "prod", "--yes", "--first-install", "mark-failed")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s\n%s", code, out, errOut)
	}
	assertCalls(t, f, "mark-failed 1")
	if statusOf(f, 1) != model.StatusFailed || !strings.Contains(out, "helm upgrade --install") {
		t.Errorf("status = %s\n%s", statusOf(f, 1), out)
	}
}

func TestFixFirstInstallUninstall(t *testing.T) {
	f := newFake(rev(1, model.StatusPendingInstall, 3*time.Hour))
	code, out, _ := run(t, f, "", false, "fix", "web", "-n", "prod", "--yes", "--first-install=uninstall")
	if code != ExitOK || !strings.Contains(out, "history are gone") {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	assertCalls(t, f, "uninstall")
	if _, ok := f.releases[key{"prod", "web"}]; ok {
		t.Error("release still exists")
	}
}

func TestFixFirstInstallStillGatedByVerdict(t *testing.T) {
	f := newFake(rev(1, model.StatusPendingInstall, 2*time.Minute))
	code, _, _ := run(t, f, "", false, "fix", "web", "-n", "prod", "--yes", "--first-install=uninstall")
	if code != ExitRefused {
		t.Errorf("a young first install must not be uninstalled, exit = %d", code)
	}
	assertCalls(t, f)
}

func TestFixNotStuck(t *testing.T) {
	f := newFake(rev(1, model.StatusDeployed, time.Hour))
	code, out, _ := run(t, f, "", false, "fix", "web", "-n", "prod", "--yes")
	if code != ExitOK || !strings.Contains(out, "is not stuck") {
		t.Errorf("exit = %d\n%s", code, out)
	}
	assertCalls(t, f)
}

func TestFixAbortsWhenReleaseChangesUnderneath(t *testing.T) {
	f := stuckUpgrade()
	fb := &lateChange{fakeBackend: f}
	var stdout, stderr bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Now: func() time.Time { return t0 }, Tool: "helm-unstick"}
	code := Run(context.Background(), []string{"fix", "web", "-n", "prod", "--yes"}, env, func(Global) (Backend, error) { return fb, nil })
	if code != ExitError || !strings.Contains(stderr.String(), "changed while") {
		t.Errorf("exit = %d, stderr = %q", code, stderr.String())
	}
	assertCalls(t, f)
}

// lateChange bumps the pending record's modifiedAt right after Inspect returns, which is
// what a freshly started operation looks like to the recheck before acting.
type lateChange struct{ *fakeBackend }

func (l *lateChange) Inspect(ctx context.Context, s *model.Stuck, now time.Time, w time.Duration) (verdict.Evidence, error) {
	ev, err := l.fakeBackend.Inspect(ctx, s, now, w)
	revs := l.releases[key{"prod", "web"}]
	revs[2].ModifiedAt = t0.Add(-time.Second)
	return ev, err
}

func TestFlagsMayFollowThePositionalArgument(t *testing.T) {
	for _, args := range [][]string{
		{"fix", "web", "-n", "prod", "--dry-run"},
		{"fix", "-n", "prod", "web", "--dry-run"},
		{"fix", "--dry-run", "--namespace=prod", "web"},
	} {
		code, out, errOut := run(t, stuckUpgrade(), "", false, args...)
		if code != ExitOK || !strings.Contains(out, "Dry run") {
			t.Errorf("%v: exit = %d\n%s\n%s", args, code, out, errOut)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		code int
	}{
		{"no command", nil, ExitError},
		{"unknown command", []string{"heal"}, ExitError},
		{"help", []string{"help"}, ExitOK},
		{"scan help", []string{"scan", "-h"}, ExitOK},
		{"fix help", []string{"fix", "--help"}, ExitOK},
		{"explain help", []string{"explain", "-h"}, ExitOK},
		{"version", []string{"version"}, ExitOK},
		{"fix without release", []string{"fix", "-n", "prod"}, ExitError},
		{"fix two releases", []string{"fix", "a", "b"}, ExitError},
		{"bad first-install", []string{"fix", "web", "--first-install", "delete"}, ExitError},
		{"bad strategy", []string{"fix", "web", "--strategy", "yolo"}, ExitError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, _ := run(t, stuckUpgrade(), "", false, tt.args...)
			if code != tt.code {
				t.Errorf("exit = %d, want %d", code, tt.code)
			}
		})
	}
}

func TestHelpMentionsExitCodes(t *testing.T) {
	_, out, _ := run(t, stuckUpgrade(), "", false, "help")
	for _, want := range []string{"scan", "explain", "fix", "Exit codes", "1  error", "2  scan found", "3  fix refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q", want)
		}
	}
	if strings.Contains(out, "\u2014") {
		t.Error("help must not contain em dashes")
	}
}

func TestToolNameIsUsedInOutput(t *testing.T) {
	f := newFake(rev(1, model.StatusPendingInstall, 3*time.Hour))
	var stdout, stderr bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Now: func() time.Time { return t0 }, Tool: "kubectl unstick"}
	Run(context.Background(), []string{"fix", "web", "-n", "prod"}, env, func(Global) (Backend, error) { return f, nil })
	if !strings.Contains(stdout.String(), "kubectl unstick fix web -n prod --first-install=mark-failed") {
		t.Errorf("commands must use the invoked name:\n%s", stdout.String())
	}
}

func TestBackendFactoryFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Now: func() time.Time { return t0 }}
	code := Run(context.Background(), []string{"scan"}, env, func(Global) (Backend, error) { return nil, errors.New("no kubeconfig") })
	if code != ExitError || !strings.Contains(stderr.String(), "no kubeconfig") {
		t.Errorf("exit = %d, stderr = %q", code, stderr.String())
	}
}

func TestGlobalFlagsReachTheFactory(t *testing.T) {
	var got Global
	var stdout, stderr bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Now: func() time.Time { return t0 }}
	Run(context.Background(), []string{"scan", "-n", "prod", "--kube-context", "kind-e2e", "--kubeconfig", "/tmp/kc", "--driver", "configmap"}, env,
		func(g Global) (Backend, error) { got = g; return newFake(), nil })
	want := Global{Namespace: "prod", KubeContext: "kind-e2e", KubeConfig: "/tmp/kc", Driver: "configmap"}
	if got != want {
		t.Errorf("Global = %+v, want %+v", got, want)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"kind-e2e": "kind-e2e", "": "''", "my ctx": "'my ctx'", "it's": `'it'\''s'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
