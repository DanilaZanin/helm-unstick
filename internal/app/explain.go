package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/DanilaZanin/helm-unstick/internal/model"
	"github.com/DanilaZanin/helm-unstick/internal/plan"
	"github.com/DanilaZanin/helm-unstick/internal/render"
)

const explainHelp = `Usage: explain RELEASE [-n NAMESPACE] [--older-than 10m]

Prints why the release is stuck, the liveness verdict with its reasons, the recovery
plan with exact commands, and the revision history. Changes nothing.
`

func runExplain(ctx context.Context, args []string, env Env, newBackend Factory) int {
	var c commonFlags
	older := newDurationFlag(DefaultOlderThan)
	fs := newFlagSet("explain")
	c.bind(fs)
	fs.Var(older, "older-than", "")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return parseFailure(err, env, "explain", explainHelp)
	}
	if len(pos) != 1 {
		return fail(env, "explain takes exactly one release name")
	}
	if err := c.validate(); err != nil {
		return fail(env, "%v", err)
	}
	be, err := newBackend(c.Global)
	if err != nil {
		return fail(env, "%v", err)
	}
	ns := c.Namespace
	if ns == "" {
		ns = be.Namespace()
	}
	h, err := be.History(ctx, ns, pos[0])
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
	render.Explain(env.Stdout, render.Report{
		Stuck: s, Age: a.Age, OlderThan: older.d, Verdict: a.Result,
		Plan: plan.Build(s, plan.FirstInstallNone, pctx),
	})
	return ExitOK
}

func historyFailure(env Env, err error) int {
	if errors.Is(err, model.ErrNotFound) {
		return fail(env, "%v", err)
	}
	return fail(env, "reading release history: %v", err)
}

func notStuck(env Env, h model.History) {
	last, _ := h.Latest()
	fmt.Fprintf(env.Stdout, "Release %s in namespace %s is not stuck: revision %d has status %s. Nothing to do.\n",
		h.Release, h.Namespace, last.Number, last.Status)
}
