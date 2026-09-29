// Package verdict decides whether a pending Helm release is safe to touch.
//
// Helm has no lock. "Busy" is nothing more than the pending-* status of the latest
// revision, and the age of that record does not prove the operation is dead. The verdict
// therefore needs positive evidence before it says a release is stale, and any sign of
// life, or any check that could not be completed, blocks it.
package verdict

import (
	"fmt"
	"strings"
	"time"

	"github.com/DanilaZanin/helm-unstick/internal/humanize"
)

// Verdict is the liveness verdict for a pending release.
type Verdict string

// The three possible verdicts.
const (
	// Stale means the pending record is older than the threshold and nothing shows activity.
	Stale Verdict = "stale"
	// PossiblyRunning means the operation may still be running. fix never acts on it.
	PossiblyRunning Verdict = "possibly-running"
	// Unknown means the evidence could not be collected. fix acts only with --force-unknown.
	Unknown Verdict = "unknown"
)

// SignalKind classifies a sign of activity.
type SignalKind string

// Signal kinds.
const (
	SignalRecentChange SignalKind = "recent-change"
	SignalJobRunning   SignalKind = "job-running"
	SignalRollout      SignalKind = "rollout-in-progress"
	SignalNotReady     SignalKind = "not-ready"
)

// Signal is one positive sign that the operation may still be running.
type Signal struct {
	Kind   SignalKind
	Object string // for example Deployment/web
	Detail string
	// Bounded marks a signal that only says "Helm --wait may still be waiting on this
	// object". Helm applies --timeout to the pre-hooks, the wait and the post-hooks one
	// after another, so the signal stops counting once the pending record is older than
	// WaitWindow(--helm-timeout).
	Bounded bool
	// Permanent marks real work in progress: an active Job or a running hook Pod.
	// Helm's timeout does not end it (the workload keeps running after helm is killed), so it
	// never expires. The user waits for it or deletes it.
	Permanent bool
}

func (s Signal) String() string { return s.Object + ": " + s.Detail }

// Evidence is everything collected about the cluster side of a pending release.
type Evidence struct {
	Signals []Signal
	// Errors lists checks that could not be completed (missing permissions, API errors).
	Errors []string
	// Checked counts the release objects that were looked at.
	Checked int
}

// Input is what Decide needs.
type Input struct {
	Age         time.Duration // time since the pending revision was last written
	OlderThan   time.Duration // the --older-than threshold, also the recent-change window
	HelmTimeout time.Duration // the --timeout of the Helm run that may still be alive (--helm-timeout)
	Evidence    Evidence
}

// HelmGrace is added to the Helm wait window before a not-ready object stops counting: it
// covers clock skew and the moment Helm needs to notice the timeout and write its final record.
const HelmGrace = time.Minute

// WaitWindow is how long a live helm can legitimately keep a release pending without any
// object changing: --timeout applies to the pre-hooks, to the wait and to the post-hooks
// separately (pkg/action/upgrade.go), so the worst case is three timeouts, plus the grace.
func WaitWindow(helmTimeout time.Duration) time.Duration { return 3*helmTimeout + HelmGrace }

// Result is the verdict plus the human-readable reasons behind it.
type Result struct {
	Verdict Verdict
	Reasons []string
}

// Decide applies the rules in order: any sign of life wins, then incomplete evidence,
// and only then stale.
func Decide(in Input) Result {
	age, skewed := in.Age, false
	if age < 0 {
		age, skewed = 0, true
	}
	var res Result
	running := false
	if age < in.OlderThan {
		running = true
		res.Reasons = append(res.Reasons, fmt.Sprintf(
			"the pending revision was written %s ago, which is less than the %s threshold",
			humanize.Duration(age), humanize.Duration(in.OlderThan)))
	}
	if skewed {
		res.Reasons = append(res.Reasons, "the pending revision is timestamped in the future (clock skew between this machine and the cluster?)")
	}
	waitLimit := WaitWindow(in.HelmTimeout)
	var expired []string
	for _, s := range in.Evidence.Signals {
		switch {
		case s.Permanent:
			running = true
			res.Reasons = append(res.Reasons, fmt.Sprintf(
				"%s [real work is running and no timeout ends it: wait for it to finish or delete the Job/Pod if it is orphaned]", s))
		case s.Bounded && age > waitLimit:
			expired = append(expired, s.Object)
		case s.Bounded:
			running = true
			res.Reasons = append(res.Reasons, fmt.Sprintf(
				"%s [a live helm may still be waiting: the record is younger than 3 x --helm-timeout %s plus %s grace, the worst case of pre-hooks, wait and post-hooks; set --helm-timeout to the --timeout of your deploy]",
				s, humanize.Duration(in.HelmTimeout), humanize.Duration(HelmGrace)))
		default:
			running = true
			res.Reasons = append(res.Reasons, s.String())
		}
	}
	if running {
		res.Verdict = PossiblyRunning
		return res
	}
	if len(in.Evidence.Errors) > 0 {
		res.Verdict = Unknown
		for _, e := range in.Evidence.Errors {
			res.Reasons = append(res.Reasons, "could not check: "+e)
		}
		return res
	}
	res.Verdict = Stale
	res.Reasons = append(res.Reasons,
		fmt.Sprintf("the pending revision was written %s ago, more than the %s threshold",
			humanize.Duration(age), humanize.Duration(in.OlderThan)))
	switch {
	case in.OlderThan == 0:
		res.Reasons = append(res.Reasons, "the recent-change window is 0s, so object modification times were not considered")
	case in.Evidence.Checked == 0:
		res.Reasons = append(res.Reasons, "none of the release objects exists in the cluster, so there is nothing that could still be changing")
	default:
		res.Reasons = append(res.Reasons, fmt.Sprintf(
			"none of the %d release objects found in the cluster changed in the last %s", in.Evidence.Checked, humanize.Duration(in.OlderThan)))
	}
	if len(expired) > 0 {
		res.Reasons = append(res.Reasons, fmt.Sprintf(
			"%s not ready, but the record is older than 3 x the Helm timeout (%s) plus grace, so any helm that waited on them has given up",
			strings.Join(expired, ", "), humanize.Duration(in.HelmTimeout)))
	} else {
		res.Reasons = append(res.Reasons, "nothing Helm --wait would wait on (Pods, Jobs, rollouts, volumes, load balancers) is pending")
	}
	return res
}

// Allows reports whether fix may modify the release. why explains a refusal, or notes an override.
func (r Result) Allows(forceUnknown bool) (ok bool, why string) {
	switch r.Verdict {
	case Stale:
		return true, ""
	case Unknown:
		if forceUnknown {
			return true, "acting on an unknown verdict because --force-unknown was given: liveness could not be verified"
		}
		return false, "liveness could not be verified, so the release is left alone (--force-unknown overrides this for an unknown verdict)"
	default:
		return false, "the operation may still be running, so the release is left alone (no flag overrides this: wait and run the command again)"
	}
}
