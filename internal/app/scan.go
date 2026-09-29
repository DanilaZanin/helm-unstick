package app

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/DanilaZanin/helm-unstick/internal/model"
	"github.com/DanilaZanin/helm-unstick/internal/plan"
	"github.com/DanilaZanin/helm-unstick/internal/render"
	"github.com/DanilaZanin/helm-unstick/internal/verdict"
)

// assessment is a stuck release together with the liveness verdict.
type assessment struct {
	Stuck    *model.Stuck
	Age      time.Duration
	Evidence verdict.Evidence
	Result   verdict.Result
}

// assess collects liveness evidence for a stuck release and decides the verdict.
func assess(ctx context.Context, be Backend, s *model.Stuck, olderThan time.Duration, now time.Time) assessment {
	ev, err := be.Inspect(ctx, s, now, olderThan)
	if err != nil {
		ev.Errors = append(ev.Errors, err.Error())
	}
	var age time.Duration
	if last := s.Pending.LastActivity(); last.IsZero() {
		ev.Errors = append(ev.Errors, "the pending revision carries no timestamp, so its age is unknown")
	} else {
		age = now.Sub(last)
	}
	res := verdict.Decide(verdict.Input{Age: age, OlderThan: olderThan, Evidence: ev})
	return assessment{Stuck: s, Age: age, Evidence: ev, Result: res}
}

const scanHelp = `Usage: scan [-n NAMESPACE | -A] [--older-than 10m] [-o table|json]

Lists releases whose latest revision is pending-install, pending-upgrade or
pending-rollback, with a liveness verdict for each. Exit code 2 when any is found.

  -n, --namespace NS     namespace to scan (default: namespace of the kube context)
  -A, --all-namespaces   scan every namespace
  --older-than DURATION  pending records younger than this, and recent activity inside
                         this window, count as "possibly running" (default 10m)
  -o, --output FORMAT    table (default) or json
`

func runScan(ctx context.Context, args []string, env Env, newBackend Factory) int {
	var (
		c      commonFlags
		all    bool
		output string
	)
	older := newDurationFlag(DefaultOlderThan)
	fs := newFlagSet("scan")
	c.bind(fs)
	fs.BoolVar(&all, "A", false, "")
	fs.BoolVar(&all, "all-namespaces", false, "")
	fs.Var(older, "older-than", "")
	fs.StringVar(&output, "o", "table", "")
	fs.StringVar(&output, "output", "table", "")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return parseFailure(err, env, "scan", scanHelp)
	}
	switch {
	case len(pos) > 0:
		return fail(env, "scan takes no arguments, got %q", pos[0])
	case all && c.Namespace != "":
		return fail(env, "-A and -n cannot be combined")
	case output != "table" && output != "json":
		return fail(env, "unsupported output %q: use table or json", output)
	}
	if err := c.validate(); err != nil {
		return fail(env, "%v", err)
	}
	be, err := newBackend(c.Global)
	if err != nil {
		return fail(env, "%v", err)
	}

	ns := c.Namespace
	if ns == "" && !all {
		ns = be.Namespace()
	}
	histories, err := be.ListPending(ctx, ns)
	if err != nil {
		return fail(env, "listing releases: %v", err)
	}

	now := env.Now()
	var rows []render.ScanRow
	for _, h := range histories {
		s := model.Analyze(h)
		if s == nil {
			continue
		}
		a := assess(ctx, be, s, older.d, now)
		rows = append(rows, render.NewScanRow(s, a.Age, a.Result))
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}
		return rows[i].Release < rows[j].Release
	})

	if output == "json" {
		if err := render.ScanJSON(env.Stdout, rows); err != nil {
			return fail(env, "writing output: %v", err)
		}
	} else {
		if len(rows) == 0 {
			where := "namespace " + ns
			if all {
				where = "any namespace"
			}
			fmt.Fprintf(env.Stdout, "No release is stuck in a pending-* state in %s.\n", where)
		} else {
			if err := render.ScanTable(env.Stdout, rows); err != nil {
				return fail(env, "writing output: %v", err)
			}
			fmt.Fprintf(env.Stdout, "\nRun \"%s explain RELEASE -n NAMESPACE\" for the reasons and the recovery plan.\n", env.Tool)
		}
	}
	if len(rows) > 0 {
		return ExitStuck
	}
	return ExitOK
}

// planContext builds the plan.Context that repeats the flags the user passed.
func planContext(env Env, extra []string) plan.Context {
	return plan.Context{Tool: env.Tool, Flags: extra}
}
