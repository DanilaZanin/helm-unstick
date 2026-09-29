// Package liveness turns the live state of a release object into signs of activity.
//
// It works on plain map[string]interface{} objects (what the Kubernetes API returns as
// JSON), so it needs no client library and can be tested with literals.
package liveness

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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
	// GenerateName is set instead of Name for an object whose name Kubernetes chooses; the
	// live objects are the ones whose names start with it.
	GenerateName string
	Hook         bool // the object comes from a Helm hook manifest
	// Labels are the labels of the rendered manifest as sorted "key=value" pairs joined by
	// commas. A generated-name object that carries one of them with the release name as its
	// value is known to belong to the release.
	Labels string
}

func (r Ref) String() string {
	if r.Name == "" {
		return r.Kind + "/" + r.GenerateName + "*"
	}
	return r.Kind + "/" + r.Name
}

// Inspect returns the signs of activity found on one live object. window is how far back a
// modification still counts as activity.
//
// Besides recent modifications it answers one question: would `helm ... --wait` still be
// waiting on this object? The rules follow Helm 3's ReadyChecker (pkg/kube/ready.go) and,
// where kubectl rollout status is stricter, kubectl. Being stricter is the safe direction:
// every such signal expires with Helm's own timeout (see verdict.Signal.Bounded), except an
// active hook Job or running hook Pod, which is real work and never expires (Signal.Permanent).
func Inspect(ref Ref, obj map[string]interface{}, now time.Time, window time.Duration) []verdict.Signal {
	var out []verdict.Signal
	if at, manager, ok := LatestModification(obj); ok && window > 0 && now.Sub(at) < window {
		detail := fmt.Sprintf("modified %s ago", humanize.Duration(now.Sub(at)))
		if manager != "" {
			detail += " by " + manager
		}
		out = append(out, verdict.Signal{Kind: verdict.SignalRecentChange, Object: ref.String(), Detail: detail})
	}
	switch {
	case isKind(ref, "Deployment", "apps", "extensions"):
		if why, busy := deploymentBusy(obj); busy {
			out = append(out, rollout(ref, why))
		}
	case isKind(ref, "StatefulSet", "apps"):
		if why, busy := statefulSetBusy(obj); busy {
			out = append(out, rollout(ref, why))
		}
	case isKind(ref, "DaemonSet", "apps", "extensions"):
		if why, busy := daemonSetBusy(obj); busy {
			out = append(out, rollout(ref, why))
		}
	case isKind(ref, "Job", "batch"):
		if why, busy := jobBusy(obj); busy {
			sig := jobSignal(ref, why)
			if active, _ := intAt(obj, "status", "active"); ref.Hook && active > 0 {
				sig = permanent(sig)
			}
			out = append(out, sig)
		}
	case isKind(ref, "Pod", ""):
		why, busy := podBusy(obj, ref.Hook)
		if busy && ref.Hook {
			sig := jobSignal(ref, why)
			if phase, _ := stringAt(obj, "status", "phase"); phase == "Running" {
				sig = permanent(sig)
			}
			out = append(out, sig)
		} else if busy {
			out = append(out, notReady(ref, why))
		}
	case isKind(ref, "PersistentVolumeClaim", ""):
		if phase, _ := stringAt(obj, "status", "phase"); phase != "Bound" {
			out = append(out, notReady(ref, "claim is "+orUnset(phase)+", not Bound"))
		}
	case isKind(ref, "Service", ""):
		if why, busy := serviceBusy(obj); busy {
			out = append(out, notReady(ref, why))
		}
	case isKind(ref, "ReplicaSet", "apps", "extensions"), isKind(ref, "ReplicationController", ""):
		if why, busy := replicaSetBusy(obj); busy {
			out = append(out, notReady(ref, why))
		}
	case isKind(ref, "CustomResourceDefinition", "apiextensions.k8s.io"):
		if why, busy := crdBusy(obj); busy {
			out = append(out, notReady(ref, why))
		}
	case isCustom(ref):
		if why, busy := customBusy(obj); busy {
			out = append(out, notReady(ref, why))
		}
	}
	return out
}

func rollout(ref Ref, why string) verdict.Signal {
	return verdict.Signal{Kind: verdict.SignalRollout, Object: ref.String(), Detail: "rollout in progress: " + why, Bounded: true}
}

func notReady(ref Ref, why string) verdict.Signal {
	return verdict.Signal{Kind: verdict.SignalNotReady, Object: ref.String(), Detail: "helm --wait may still be waiting: " + why, Bounded: true}
}

func jobSignal(ref Ref, why string) verdict.Signal {
	what := "has not finished"
	if ref.Hook {
		what = "hook has not finished"
	}
	return verdict.Signal{Kind: verdict.SignalJobRunning, Object: ref.String(), Detail: what + ": " + why, Bounded: true}
}

// permanent turns a hook signal into real work that no timeout ends: the workload runs on
// after the helm that started it is gone.
func permanent(s verdict.Signal) verdict.Signal {
	s.Bounded, s.Permanent = false, true
	return s
}

func orUnset(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}

// isKind matches a built-in kind. An empty apiVersion (as in hand-written test refs) matches
// any group; otherwise the group must be one of groups, so a CRD that reuses a kind name
// such as Service is not judged by the core rules.
func isKind(ref Ref, kind string, groups ...string) bool {
	if ref.Kind != kind {
		return false
	}
	if ref.APIVersion == "" {
		return true
	}
	group := ""
	if g, _, ok := strings.Cut(ref.APIVersion, "/"); ok {
		group = g
	}
	return slices.Contains(groups, group)
}

// isCustom reports a group that is not part of Kubernetes itself, so the object is a custom resource.
func isCustom(ref Ref) bool {
	g, _, ok := strings.Cut(ref.APIVersion, "/")
	return ok && strings.Contains(g, ".") && !strings.HasSuffix(g, ".k8s.io")
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

// deploymentBusy mirrors `kubectl rollout status`: the controller must have seen the latest
// spec before any condition is trusted. A rollout past its progress deadline still counts:
// Helm 3 --wait keeps waiting after ProgressDeadlineExceeded (Helm 4 stops).
func deploymentBusy(obj map[string]interface{}) (string, bool) {
	if paused, _ := boolAt(obj, "spec", "paused"); paused {
		return "", false
	}
	if why, busy := generationLag(obj); busy {
		return why, true
	}
	want := specReplicas(obj)
	updated, _ := intAt(obj, "status", "updatedReplicas")
	total, _ := intAt(obj, "status", "replicas")
	available, _ := intAt(obj, "status", "availableReplicas")
	var why string
	switch {
	case updated < want:
		why = fmt.Sprintf("%d of %d replicas updated", updated, want)
	case total > updated:
		why = fmt.Sprintf("%d old replicas still terminating", total-updated)
	case available < updated:
		why = fmt.Sprintf("%d of %d updated replicas available", available, updated)
	default:
		return "", false
	}
	for _, c := range conditions(obj) {
		if c.Type == "Progressing" && c.Status == "False" && c.Reason == "ProgressDeadlineExceeded" {
			why += " (progress deadline exceeded, but Helm 3 --wait keeps waiting)"
		}
	}
	return why, true
}

// statefulSetBusy follows Helm's ReadyChecker, including partitioned rollouts: with
// replicas 3 and partition 2 a finished rollout has one updated pod.
func statefulSetBusy(obj map[string]interface{}) (string, bool) {
	if t, ok := stringAt(obj, "spec", "updateStrategy", "type"); ok && t != "RollingUpdate" {
		return "", false
	}
	if why, busy := generationLag(obj); busy {
		return why, true
	}
	want := specReplicas(obj)
	partition, _ := intAt(obj, "spec", "updateStrategy", "rollingUpdate", "partition")
	updated, _ := intAt(obj, "status", "updatedReplicas")
	ready, _ := intAt(obj, "status", "readyReplicas")
	switch {
	case updated < want-partition:
		return fmt.Sprintf("%d of %d replicas updated (partition %d)", updated, want, partition), true
	case ready != want:
		return fmt.Sprintf("%d of %d replicas ready", ready, want), true
	}
	if partition == 0 {
		current, _ := stringAt(obj, "status", "currentRevision")
		update, _ := stringAt(obj, "status", "updateRevision")
		if current != update {
			return fmt.Sprintf("currentRevision %s has not reached updateRevision %s", current, update), true
		}
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

// podBusy: a hook Pod is done at Succeeded or Failed (Helm waits for the phase); any other
// Pod is waited on until its Ready condition is true.
func podBusy(obj map[string]interface{}, hook bool) (string, bool) {
	phase, _ := stringAt(obj, "status", "phase")
	if hook {
		switch phase {
		case "Succeeded", "Failed":
			return "", false
		case "":
			return "pod has no phase yet", true
		}
		return "pod phase is " + phase, true
	}
	for _, c := range conditions(obj) {
		if c.Type == "Ready" && c.Status == "True" {
			return "", false
		}
	}
	return "pod is not Ready (phase " + orUnset(phase) + ")", true
}

// serviceBusy: Helm waits for a cluster IP and, for a LoadBalancer without external IPs,
// for an ingress entry.
func serviceBusy(obj map[string]interface{}) (string, bool) {
	typ, _ := stringAt(obj, "spec", "type")
	if typ == "ExternalName" {
		return "", false
	}
	if ip, _ := stringAt(obj, "spec", "clusterIP"); ip == "" {
		return "service has no cluster IP yet", true
	}
	if typ == "LoadBalancer" {
		if ext, ok := lookup(obj, "spec", "externalIPs"); ok {
			if l, _ := ext.([]interface{}); len(l) > 0 {
				return "", false
			}
		}
		if ing, ok := lookup(obj, "status", "loadBalancer", "ingress"); !ok {
			return "load balancer has no ingress address yet", true
		} else if l, _ := ing.([]interface{}); len(l) == 0 {
			return "load balancer has no ingress address yet", true
		}
	}
	return "", false
}

func replicaSetBusy(obj map[string]interface{}) (string, bool) {
	if why, busy := generationLag(obj); busy {
		return why, true
	}
	want := specReplicas(obj)
	if ready, _ := intAt(obj, "status", "readyReplicas"); ready < want {
		return fmt.Sprintf("%d of %d replicas ready", ready, want), true
	}
	return "", false
}

func crdBusy(obj map[string]interface{}) (string, bool) {
	for _, c := range conditions(obj) {
		switch {
		case c.Type == "Established" && c.Status == "True":
			return "", false
		case c.Type == "NamesAccepted" && c.Status == "False":
			return "", false // a naming conflict: Helm stops waiting
		}
	}
	return "CRD is not Established", true
}

// customBusy reads the conventional conditions of a custom resource: Reconciling (kstatus,
// which Helm 4 waits on) and Ready.
func customBusy(obj map[string]interface{}) (string, bool) {
	for _, c := range conditions(obj) {
		switch {
		case c.Type == "Reconciling" && c.Status == "True":
			return "resource reports Reconciling=True", true
		case c.Type == "Ready" && c.Status != "True":
			return "resource reports Ready=" + c.Status, true
		}
	}
	return "", false
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
