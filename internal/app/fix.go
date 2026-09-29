package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/DanilaZanin/helm-unstick/internal/model"
	"github.com/DanilaZanin/helm-unstick/internal/plan"
	"github.com/DanilaZanin/helm-unstick/internal/render"
)

const fixHelp = `Usage: fix RELEASE [-n NAMESPACE] [flags]

Recovers a release stuck in a pending-* state. Acts only when the verdict is "stale".

  --dry-run              print the plan and change nothing
  -y, --yes              do not ask for confirmation
  --older-than DURATION  staleness threshold and recent-activity window (default 10m)
  --force-unknown        act although liveness could not be verified. It never
                         overrides "possibly-running"
  --first-install MODE   what to do when there is no deployed revision to return to:
                         mark-failed  set the pending revision to failed and keep the resources
                         uninstall    remove the release and its history
                         Without it, fix only prints the plan
  --strategy S           how to roll back a pending revision:
                         auto (default)  try the rollback directly, and if Helm refuses while
                                         the revision is untouched, mark it failed and retry
                         direct          rollback only
                         mark-failed     mark the pending revision failed, then roll back
  --wait                 wait for the rollback/uninstall to become ready
  --timeout DURATION     timeout for hooks and --wait (default 5m)

Exit codes: 0 done, 1 error, 2 still stuck, waiting for your --first-install choice,
3 refused because the operation may be running or liveness is unknown.
`

const interruptedReason = "Interrupted operation, marked failed by helm-unstick"

type fixOptions struct {
	dryRun       bool
	yes          bool
	forceUnknown bool
	strategy     string
	wait         bool
}

func runFix(ctx context.Context, args []string, env Env, newBackend Factory) int {
	var (
		c        commonFlags
		o        fixOptions
		firstRaw string
	)
	older := newDurationFlag(DefaultOlderThan)
	timeout := newDurationFlag(DefaultTimeout)
	fs := newFlagSet("fix")
	c.bind(fs)
	fs.Var(older, "older-than", "")
	fs.Var(timeout, "timeout", "")
	fs.BoolVar(&o.dryRun, "dry-run", false, "")
	fs.BoolVar(&o.yes, "yes", false, "")
	fs.BoolVar(&o.yes, "y", false, "")
	fs.BoolVar(&o.forceUnknown, "force-unknown", false, "")
	fs.BoolVar(&o.wait, "wait", false, "")
	fs.StringVar(&firstRaw, "first-install", "", "")
	fs.StringVar(&o.strategy, "strategy", "auto", "")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return parseFailure(err, env, "fix", fixHelp)
	}
	if len(pos) != 1 {
		return fail(env, "fix takes exactly one release name")
	}
	if err := c.validate(); err != nil {
		return fail(env, "%v", err)
	}
	mode, err := plan.ParseFirstInstall(firstRaw)
	if err != nil {
		return fail(env, "%v", err)
	}
	switch o.strategy {
	case "auto", "direct", "mark-failed":
	default:
		return fail(env, "unsupported --strategy %q: use auto, direct or mark-failed", o.strategy)
	}
	be, err := newBackend(c.Global)
	if err != nil {
		return fail(env, "%v", err)
	}
	ns := c.Namespace
	if ns == "" {
		ns = be.Namespace()
	}
	release := pos[0]

	h, err := be.History(ctx, ns, release)
	if err != nil {
		return historyFailure(env, err)
	}
	s := model.Analyze(h)
	if s == nil {
		notStuck(env, h)
		return ExitOK
	}

	a := assess(ctx, be, s, older.d, env.Now())
	pctx := planContext(env, passthrough(fs, "older-than", "kube-context", "kubeconfig", "driver"))
	p := plan.Build(s, mode, pctx)
	report := render.Report{Stuck: s, Age: a.Age, OlderThan: older.d, Verdict: a.Result, Plan: p}
	render.Summary(env.Stdout, report)
	fmt.Fprintln(env.Stdout)

	ok, why := a.Result.Allows(o.forceUnknown)
	if !ok {
		fmt.Fprintf(env.Stdout, "Refused:   %s\n", why)
		fmt.Fprintf(env.Stdout, "           Run \"%s explain %s -n %s\" for the full picture.\n", env.Tool, release, ns)
		return ExitRefused
	}
	if why != "" {
		fmt.Fprintf(env.Stdout, "Warning:   %s\n\n", why)
	}

	render.Plan(env.Stdout, p)
	if p.Action == plan.ActionChoose {
		fmt.Fprintf(env.Stdout, "\nNothing was changed. Pick one of the commands above to continue.\n")
		return ExitStuck
	}
	if o.dryRun {
		fmt.Fprintf(env.Stdout, "\nDry run: nothing was changed.\n")
		return ExitOK
	}
	if !o.yes {
		if !env.Interactive {
			return fail(env, "refusing to change the release without --yes when stdin is not a terminal")
		}
		fmt.Fprintln(env.Stdout)
		confirmed, err := confirm(env, "Proceed?")
		if err != nil {
			return fail(env, "reading the answer: %v", err)
		}
		if !confirmed {
			fmt.Fprintln(env.Stdout, "Aborted. Nothing was changed.")
			return ExitError
		}
	}

	fmt.Fprintln(env.Stdout)
	if err := ensureUnchanged(ctx, be, s); err != nil {
		return fail(env, "%v", err)
	}
	opts := model.ActionOptions{Wait: o.wait, Timeout: timeout.d}
	if err := execute(ctx, env, be, s, p, o, opts); err != nil {
		return fail(env, "%v", err)
	}
	return ExitOK
}

func confirm(env Env, question string) (bool, error) {
	fmt.Fprintf(env.Stdout, "%s [y/N] ", question)
	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// ensureUnchanged re-reads the history right before acting. If anything wrote to the
// release since the verdict, the verdict is void: a live operation may have started.
func ensureUnchanged(ctx context.Context, be Backend, before *model.Stuck) error {
	h, err := be.History(ctx, before.Namespace, before.Release)
	if err != nil {
		return fmt.Errorf("re-reading release history before acting: %w", err)
	}
	now := model.Analyze(h)
	if now == nil ||
		now.Pending.Number != before.Pending.Number ||
		now.Pending.Status != before.Pending.Status ||
		!now.Pending.LastActivity().Equal(before.Pending.LastActivity()) {
		return errors.New("the release changed while helm-unstick was checking it (a new operation may have started); nothing was changed, run the command again")
	}
	return nil
}

func execute(ctx context.Context, env Env, be Backend, s *model.Stuck, p plan.Plan, o fixOptions, opts model.ActionOptions) error {
	out := env.Stdout
	switch p.Action {
	case plan.ActionRollback:
		fmt.Fprintf(out, "Rolling back %s to revision %d ...\n", s.Release, p.RollbackTo)
		path, err := rollback(ctx, out, be, s, p.RollbackTo, o.strategy, opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Rollback path: %s\n", path)
		latest, err := finishRollback(ctx, out, be, s)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Done. Release %s is deployed as revision %d (content of revision %d). The next helm upgrade can proceed.\n",
			s.Release, latest.Number, p.RollbackTo)
	case plan.ActionMarkFailed:
		fmt.Fprintf(out, "Marking revision %d of %s as failed ...\n", s.Pending.Number, s.Release)
		if err := be.MarkFailed(ctx, s.Namespace, s.Release, s.Pending.Number, interruptedReason); err != nil {
			return fmt.Errorf("marking revision %d as failed: %w", s.Pending.Number, err)
		}
		latest, err := latestRevision(ctx, be, s)
		if err != nil {
			return err
		}
		if latest.Status != model.StatusFailed {
			return fmt.Errorf("revision %d has status %s after marking it failed", latest.Number, latest.Status)
		}
		fmt.Fprintf(out, "Done. Revision %d is failed. Run your usual helm upgrade --install now.\n", latest.Number)
	case plan.ActionUninstall:
		fmt.Fprintf(out, "Uninstalling %s ...\n", s.Release)
		if err := be.Uninstall(ctx, s.Namespace, s.Release, opts); err != nil {
			return fmt.Errorf("uninstalling %s: %w", s.Release, err)
		}
		if _, err := be.History(ctx, s.Namespace, s.Release); err == nil {
			fmt.Fprintf(out, "Done, but release records for %s still exist. Check them with helm history.\n", s.Release)
		} else if errors.Is(err, model.ErrNotFound) {
			fmt.Fprintf(out, "Done. Release %s and its history are gone. Run your usual helm install now.\n", s.Release)
		} else {
			return fmt.Errorf("checking the result of the uninstall: %w", err)
		}
	default:
		return fmt.Errorf("internal error: plan action %q cannot be executed", p.Action)
	}
	return nil
}

// rollback runs the rollback according to the strategy and reports which path worked.
func rollback(ctx context.Context, out io.Writer, be Backend, s *model.Stuck, target int, strategy string, opts model.ActionOptions) (string, error) {
	roll := func() error { return be.Rollback(ctx, s.Namespace, s.Release, target, opts) }
	markFirst := func() error {
		fmt.Fprintf(out, "Marking pending revision %d as failed first ...\n", s.Pending.Number)
		if err := be.MarkFailed(ctx, s.Namespace, s.Release, s.Pending.Number, interruptedReason); err != nil {
			return fmt.Errorf("marking revision %d as failed: %w", s.Pending.Number, err)
		}
		return nil
	}
	switch strategy {
	case "direct":
		if err := roll(); err != nil {
			return "", fmt.Errorf("rollback failed: %w", err)
		}
		return "direct", nil
	case "mark-failed":
		if err := markFirst(); err != nil {
			return "", err
		}
		if err := roll(); err != nil {
			return "", fmt.Errorf("rollback failed after marking revision %d failed: %w", s.Pending.Number, err)
		}
		return "mark-failed-first", nil
	}

	// auto: try the plain rollback, and fall back only while the record is still untouched.
	err := roll()
	if err == nil {
		return "direct", nil
	}
	fmt.Fprintf(out, "Direct rollback failed: %v\n", err)
	h, herr := be.History(ctx, s.Namespace, s.Release)
	if herr != nil {
		return "", fmt.Errorf("rollback failed (%w) and the history could not be re-read: %w", err, herr)
	}
	cur := model.Analyze(h)
	if cur == nil || cur.Pending.Number != s.Pending.Number || cur.Pending.Status != s.Pending.Status {
		return "", fmt.Errorf("rollback failed and the release state changed meanwhile, so it was not retried: %w", err)
	}
	if err := markFirst(); err != nil {
		return "", err
	}
	if err := roll(); err != nil {
		return "", fmt.Errorf("rollback failed again after marking revision %d failed: %w", s.Pending.Number, err)
	}
	return "direct rollback refused, mark-failed-first worked", nil
}

func latestRevision(ctx context.Context, be Backend, s *model.Stuck) (model.Revision, error) {
	h, err := be.History(ctx, s.Namespace, s.Release)
	if err != nil {
		return model.Revision{}, fmt.Errorf("re-reading release history: %w", err)
	}
	latest, ok := h.Latest()
	if !ok {
		return model.Revision{}, errors.New("release history is empty after the operation")
	}
	return latest, nil
}

// finishRollback checks that the release ended up deployed and clears pending revisions
// the rollback left behind, so the history no longer shows an operation in progress.
func finishRollback(ctx context.Context, out io.Writer, be Backend, s *model.Stuck) (model.Revision, error) {
	h, err := be.History(ctx, s.Namespace, s.Release)
	if err != nil {
		return model.Revision{}, fmt.Errorf("re-reading release history: %w", err)
	}
	latest, ok := h.Latest()
	if !ok {
		return model.Revision{}, errors.New("release history is empty after the rollback")
	}
	if latest.Status != model.StatusDeployed {
		return latest, fmt.Errorf("the rollback finished but the latest revision %d has status %s, not deployed", latest.Number, latest.Status)
	}
	for _, r := range h.Revisions {
		if !r.Status.IsPending() || r.Number >= latest.Number {
			continue
		}
		if err := be.MarkFailed(ctx, s.Namespace, s.Release, r.Number, interruptedReason); err != nil {
			fmt.Fprintf(out, "Warning: revision %d is still %s and could not be marked failed: %v\n", r.Number, r.Status, err)
			continue
		}
		fmt.Fprintf(out, "Revision %d was left %s by the interrupted operation and is now marked failed.\n", r.Number, r.Status)
	}
	return latest, nil
}
