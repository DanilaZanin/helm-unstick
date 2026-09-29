// Package liveness turns the live state of a release object into signs of activity.
//
// It works on plain map[string]interface{} objects (what the Kubernetes API returns as
// JSON), so it needs no client library and can be tested with literals.
package liveness

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/DanilaZanin/helm-unstick/internal/humanize"
	"github.com/DanilaZanin/helm-unstick/internal/verdict"
)

// Ref identifies one object that belongs to a release.
type Ref struct {
	APIVersion string
	Kind       string
	Namespace  string // empty for cluster-scoped objects
	Name       string
	Hook       bool // the object comes from a Helm hook manifest
}

func (r Ref) String() string { return r.Kind + "/" + r.Name }

// Inspect returns the signs of activity found on one live object. window is how far back a
// modification still counts as activity.
func Inspect(ref Ref, obj map[string]interface{}, now time.Time, window time.Duration) []verdict.Signal {
	var out []verdict.Signal
	if at, manager, ok := LatestModification(obj); ok && window > 0 && now.Sub(at) < window {
		detail := fmt.Sprintf("modified %s ago", humanize.Duration(now.Sub(at)))
		if manager != "" {
			detail += " by " + manager
		}
		out = append(out, verdict.Signal{Kind: verdict.SignalRecentChange, Object: ref.String(), Detail: detail})
	}
	switch ref.Kind {
	case "Deployment":
		if why, busy := deploymentBusy(obj); busy {
			out = append(out, rollout(ref, why))
		}
	case "StatefulSet":
		if why, busy := statefulSetBusy(obj); busy {
			out = append(out, rollout(ref, why))
		}
	case "DaemonSet":
		if why, busy := daemonSetBusy(obj); busy {
			out = append(out, rollout(ref, why))
		}
	case "Job":
		if why, busy := jobBusy(obj); busy {
			out = append(out, jobSignal(ref, why))
		}
	case "Pod":
		if ref.Hook {
			if why, busy := hookPodBusy(obj); busy {
				out = append(out, jobSignal(ref, why))
			}
		}
	}
	return out
}

func rollout(ref Ref, why string) verdict.Signal {
	return verdict.Signal{Kind: verdict.SignalRollout, Object: ref.String(), Detail: "rollout in progress: " + why}
}

func jobSignal(ref Ref, why string) verdict.Signal {
	what := "has not finished"
	if ref.Hook {
		what = "hook has not finished"
	}
	return verdict.Signal{Kind: verdict.SignalJobRunning, Object: ref.String(), Detail: what + ": " + why}
}

// LatestModification returns the newest managedFields timestamp and the manager that wrote it.
// Objects without managedFields fall back to their creation time.
func LatestModification(obj map[string]interface{}) (time.Time, string, bool) {
	var (
		best    time.Time
		manager string
		found   bool
	)
	if v, ok := lookup(obj, "metadata", "managedFields"); ok {
		entries, _ := v.([]interface{})
		for _, e := range entries {
			m, ok := e.(map[string]interface{})
			if !ok {
				continue
			}
			ts, _ := m["time"].(string)
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				continue
			}
			if !found || t.After(best) {
				best, found = t, true
				manager, _ = m["manager"].(string)
			}
		}
	}
	if found {
		return best, manager, true
	}
	if ts, ok := stringAt(obj, "metadata", "creationTimestamp"); ok {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return t, "", true
		}
	}
	return time.Time{}, "", false
}

// generationLag reports a controller that has not yet seen the latest spec.
func generationLag(obj map[string]interface{}) (string, bool) {
	gen, ok := intAt(obj, "metadata", "generation")
	if !ok {
		return "", false
	}
	observed, _ := intAt(obj, "status", "observedGeneration")
	if observed < gen {
		return fmt.Sprintf("the controller has not observed the latest spec (generation %d, observed %d)", gen, observed), true
	}
	return "", false
}

func specReplicas(obj map[string]interface{}) int64 {
	if n, ok := intAt(obj, "spec", "replicas"); ok {
		return n
	}
	return 1 // the API default
}

// deploymentBusy mirrors `kubectl rollout status` for Deployments. A rollout that already
// hit its progress deadline is failed, not in progress, and does not count as activity.
func deploymentBusy(obj map[string]interface{}) (string, bool) {
	if paused, _ := boolAt(obj, "spec", "paused"); paused {
		return "", false
	}
	for _, c := range conditions(obj) {
		if c.Type == "Progressing" && c.Status == "False" && c.Reason == "ProgressDeadlineExceeded" {
			return "", false
		}
	}
	if why, busy := generationLag(obj); busy {
		return why, true
	}
	want := specReplicas(obj)
	updated, _ := intAt(obj, "status", "updatedReplicas")
	total, _ := intAt(obj, "status", "replicas")
	available, _ := intAt(obj, "status", "availableReplicas")
	switch {
	case updated < want:
		return fmt.Sprintf("%d of %d replicas updated", updated, want), true
	case total > updated:
		return fmt.Sprintf("%d old replicas still terminating", total-updated), true
	case available < updated:
		return fmt.Sprintf("%d of %d updated replicas available", available, updated), true
	}
	return "", false
}

func statefulSetBusy(obj map[string]interface{}) (string, bool) {
	if t, _ := stringAt(obj, "spec", "updateStrategy", "type"); t == "OnDelete" {
		return "", false
	}
	if why, busy := generationLag(obj); busy {
		return why, true
	}
	want := specReplicas(obj)
	updated, _ := intAt(obj, "status", "updatedReplicas")
	ready, _ := intAt(obj, "status", "readyReplicas")
	switch {
	case updated < want:
		return fmt.Sprintf("%d of %d replicas updated", updated, want), true
	case ready < want:
		return fmt.Sprintf("%d of %d replicas ready", ready, want), true
	}
	return "", false
}

func daemonSetBusy(obj map[string]interface{}) (string, bool) {
	if t, _ := stringAt(obj, "spec", "updateStrategy", "type"); t == "OnDelete" {
		return "", false
	}
	if why, busy := generationLag(obj); busy {
		return why, true
	}
	desired, _ := intAt(obj, "status", "desiredNumberScheduled")
	updated, _ := intAt(obj, "status", "updatedNumberScheduled")
	available, _ := intAt(obj, "status", "numberAvailable")
	switch {
	case updated < desired:
		return fmt.Sprintf("%d of %d nodes updated", updated, desired), true
	case available < desired:
		return fmt.Sprintf("%d of %d nodes available", available, desired), true
	}
	return "", false
}

// jobBusy reports a Job that has neither completed nor failed.
func jobBusy(obj map[string]interface{}) (string, bool) {
	if suspended, _ := boolAt(obj, "spec", "suspend"); suspended {
		return "", false
	}
	for _, c := range conditions(obj) {
		if (c.Type == "Complete" || c.Type == "Failed") && c.Status == "True" {
			return "", false
		}
	}
	if active, _ := intAt(obj, "status", "active"); active > 0 {
		return fmt.Sprintf("%d active pod(s)", active), true
	}
	return "no completion or failure recorded yet", true
}

func hookPodBusy(obj map[string]interface{}) (string, bool) {
	phase, _ := stringAt(obj, "status", "phase")
	switch phase {
	case "Succeeded", "Failed":
		return "", false
	case "":
		return "pod has no phase yet", true
	}
	return "pod phase is " + phase, true
}

type condition struct{ Type, Status, Reason string }

func conditions(obj map[string]interface{}) []condition {
	v, ok := lookup(obj, "status", "conditions")
	if !ok {
		return nil
	}
	list, _ := v.([]interface{})
	var out []condition
	for _, e := range list {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		c := condition{}
		c.Type, _ = m["type"].(string)
		c.Status, _ = m["status"].(string)
		c.Reason, _ = m["reason"].(string)
		out = append(out, c)
	}
	return out
}

func lookup(obj map[string]interface{}, path ...string) (interface{}, bool) {
	var cur interface{} = obj
	for _, p := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		if cur, ok = m[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func stringAt(obj map[string]interface{}, path ...string) (string, bool) {
	v, ok := lookup(obj, path...)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func boolAt(obj map[string]interface{}, path ...string) (bool, bool) {
	v, ok := lookup(obj, path...)
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func intAt(obj map[string]interface{}, path ...string) (int64, bool) {
	v, ok := lookup(obj, path...)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}
