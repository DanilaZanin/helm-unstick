package liveness

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DanilaZanin/helm-unstick/internal/verdict"
)

var now = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// obj builds an object from JSON, the way the Kubernetes API delivers it.
func obj(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad test JSON: %v\n%s", err, s)
	}
	return m
}

func kinds(sigs []verdict.Signal) []verdict.SignalKind {
	var out []verdict.SignalKind
	for _, s := range sigs {
		out = append(out, s.Kind)
	}
	return out
}

func hasKind(sigs []verdict.Signal, k verdict.SignalKind) bool {
	for _, s := range sigs {
		if s.Kind == k {
			return true
		}
	}
	return false
}

func TestDeploymentRollout(t *testing.T) {
	tests := []struct {
		name string
		json string
		busy bool
		why  string
	}{
		{
			name: "complete",
			json: `{"metadata":{"generation":2},"spec":{"replicas":3},
				"status":{"observedGeneration":2,"replicas":3,"updatedReplicas":3,"availableReplicas":3}}`,
		},
		{
			name: "controller has not seen the new spec",
			json: `{"metadata":{"generation":3},"spec":{"replicas":3},
				"status":{"observedGeneration":2,"replicas":3,"updatedReplicas":3,"availableReplicas":3}}`,
			busy: true, why: "has not observed the latest spec",
		},
		{
			name: "replicas still being updated",
			json: `{"metadata":{"generation":2},"spec":{"replicas":3},
				"status":{"observedGeneration":2,"replicas":4,"updatedReplicas":1,"availableReplicas":3}}`,
			busy: true, why: "1 of 3 replicas updated",
		},
		{
			name: "old replicas terminating",
			json: `{"metadata":{"generation":2},"spec":{"replicas":2},
				"status":{"observedGeneration":2,"replicas":3,"updatedReplicas":2,"availableReplicas":2}}`,
			busy: true, why: "1 old replicas still terminating",
		},
		{
			name: "new pods not yet available (slow readiness)",
			json: `{"metadata":{"generation":2},"spec":{"replicas":1},
				"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"availableReplicas":0,"unavailableReplicas":1}}`,
			busy: true, why: "0 of 1 updated replicas available",
		},
		{
			name: "replicas default to one",
			json: `{"metadata":{"generation":1},"spec":{},
				"status":{"observedGeneration":1,"replicas":1,"updatedReplicas":1,"availableReplicas":1}}`,
		},
		{
			name: "scaled to zero is complete",
			json: `{"metadata":{"generation":4},"spec":{"replicas":0},"status":{"observedGeneration":4}}`,
		},
		{
			// Helm 3 --wait keeps waiting after the progress deadline: it must still count.
			name: "progress deadline exceeded does not end Helm's wait",
			json: `{"metadata":{"generation":2},"spec":{"replicas":1},
				"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"availableReplicas":0,
				"conditions":[{"type":"Progressing","status":"False","reason":"ProgressDeadlineExceeded"}]}}`,
			busy: true, why: "progress deadline exceeded",
		},
		{
			name: "stale deadline condition does not hide a new generation",
			json: `{"metadata":{"generation":3},"spec":{"replicas":1},
				"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"availableReplicas":1,
				"conditions":[{"type":"Progressing","status":"False","reason":"ProgressDeadlineExceeded"}]}}`,
			busy: true, why: "has not observed the latest spec",
		},
		{
			name: "complete rollout with a leftover deadline condition",
			json: `{"metadata":{"generation":2},"spec":{"replicas":1},
				"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"availableReplicas":1,
				"conditions":[{"type":"Progressing","status":"False","reason":"ProgressDeadlineExceeded"}]}}`,
		},
		{
			name: "paused deployment is not rolling out",
			json: `{"metadata":{"generation":2},"spec":{"replicas":1,"paused":true},"status":{"observedGeneration":1}}`,
		},
		{
			name: "brand-new deployment with empty status",
			json: `{"metadata":{"generation":1},"spec":{"replicas":1},"status":{}}`,
			busy: true, why: "has not observed the latest spec",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sigs := Inspect(Ref{Kind: "Deployment", Name: "web"}, obj(t, tt.json), now, 0)
			if got := hasKind(sigs, verdict.SignalRollout); got != tt.busy {
				t.Fatalf("rollout signal = %v, want %v (signals: %v)", got, tt.busy, sigs)
			}
			if tt.busy && !strings.Contains(sigs[0].Detail, tt.why) {
				t.Errorf("detail %q does not contain %q", sigs[0].Detail, tt.why)
			}
			if tt.busy && sigs[0].Object != "Deployment/web" {
				t.Errorf("object = %q", sigs[0].Object)
			}
		})
	}
}

func TestStatefulSetRollout(t *testing.T) {
	tests := []struct {
		name string
		json string
		busy bool
	}{
		{"complete", `{"metadata":{"generation":1},"spec":{"replicas":3},"status":{"observedGeneration":1,"updatedReplicas":3,"readyReplicas":3}}`, false},
		{"pods not ready", `{"metadata":{"generation":1},"spec":{"replicas":3},"status":{"observedGeneration":1,"updatedReplicas":3,"readyReplicas":2}}`, true},
		{"pods not updated", `{"metadata":{"generation":2},"spec":{"replicas":3},"status":{"observedGeneration":2,"updatedReplicas":1,"readyReplicas":3}}`, true},
		{"OnDelete strategy never rolls by itself", `{"metadata":{"generation":2},"spec":{"replicas":3,"updateStrategy":{"type":"OnDelete"}},"status":{"observedGeneration":1}}`, false},
		{"finished partitioned rollout (replicas 3, partition 2, one pod updated)",
			`{"metadata":{"generation":1},"spec":{"replicas":3,"updateStrategy":{"type":"RollingUpdate","rollingUpdate":{"partition":2}}},
			"status":{"observedGeneration":1,"updatedReplicas":1,"readyReplicas":3,"currentRevision":"a","updateRevision":"b"}}`, false},
		{"partitioned rollout still short of its updated pods",
			`{"metadata":{"generation":1},"spec":{"replicas":3,"updateStrategy":{"type":"RollingUpdate","rollingUpdate":{"partition":2}}},
			"status":{"observedGeneration":1,"updatedReplicas":0,"readyReplicas":3,"currentRevision":"a","updateRevision":"b"}}`, true},
		{"unpartitioned rollout whose pods carry two revisions",
			`{"metadata":{"generation":1},"spec":{"replicas":3},
			"status":{"observedGeneration":1,"updatedReplicas":3,"readyReplicas":3,"currentRevision":"a","updateRevision":"b"}}`, true},
		{"unpartitioned rollout on one revision",
			`{"metadata":{"generation":1},"spec":{"replicas":3},
			"status":{"observedGeneration":1,"updatedReplicas":3,"readyReplicas":3,"currentRevision":"a","updateRevision":"a"}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sigs := Inspect(Ref{Kind: "StatefulSet", Name: "db"}, obj(t, tt.json), now, 0)
			if got := hasKind(sigs, verdict.SignalRollout); got != tt.busy {
				t.Errorf("rollout signal = %v, want %v", got, tt.busy)
			}
		})
	}
}

func TestDaemonSetRollout(t *testing.T) {
	tests := []struct {
		name string
		json string
		busy bool
	}{
		{"complete", `{"metadata":{"generation":1},"status":{"observedGeneration":1,"desiredNumberScheduled":3,"updatedNumberScheduled":3,"numberAvailable":3}}`, false},
		{"nodes not updated", `{"metadata":{"generation":1},"status":{"observedGeneration":1,"desiredNumberScheduled":3,"updatedNumberScheduled":1,"numberAvailable":3}}`, true},
		{"nodes not available", `{"metadata":{"generation":1},"status":{"observedGeneration":1,"desiredNumberScheduled":3,"updatedNumberScheduled":3,"numberAvailable":2}}`, true},
		{"no nodes match", `{"metadata":{"generation":1},"status":{"observedGeneration":1}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sigs := Inspect(Ref{Kind: "DaemonSet", Name: "agent"}, obj(t, tt.json), now, 0)
			if got := hasKind(sigs, verdict.SignalRollout); got != tt.busy {
				t.Errorf("rollout signal = %v, want %v", got, tt.busy)
			}
		})
	}
}

func TestJob(t *testing.T) {
	tests := []struct {
		name string
		json string
		hook bool
		busy bool
		why  string
	}{
		{"active", `{"status":{"active":1}}`, true, true, "hook has not finished: 1 active pod(s)"},
		{"active, not a hook", `{"status":{"active":2}}`, false, true, "has not finished: 2 active pod(s)"},
		{"just created", `{"status":{}}`, true, true, "no completion or failure recorded yet"},
		{"complete", `{"status":{"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}}`, true, false, ""},
		{"failed", `{"status":{"failed":6,"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}}`, true, false, ""},
		{"complete condition false does not count", `{"status":{"active":1,"conditions":[{"type":"Complete","status":"False"}]}}`, true, true, "1 active pod(s)"},
		{"suspended", `{"spec":{"suspend":true},"status":{}}`, true, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sigs := Inspect(Ref{Kind: "Job", Name: "migrate", Hook: tt.hook}, obj(t, tt.json), now, 0)
			if got := hasKind(sigs, verdict.SignalJobRunning); got != tt.busy {
				t.Fatalf("job signal = %v, want %v (signals %v)", got, tt.busy, sigs)
			}
			if tt.busy && !strings.Contains(sigs[0].Detail, tt.why) {
				t.Errorf("detail %q does not contain %q", sigs[0].Detail, tt.why)
			}
		})
	}
}

func TestHookPod(t *testing.T) {
	tests := []struct {
		name string
		json string
		hook bool
		busy bool
	}{
		{"running hook pod", `{"status":{"phase":"Running"}}`, true, true},
		{"pending hook pod", `{"status":{"phase":"Pending"}}`, true, true},
		{"new hook pod without status", `{}`, true, true},
		{"succeeded hook pod", `{"status":{"phase":"Succeeded"}}`, true, false},
		{"failed hook pod", `{"status":{"phase":"Failed"}}`, true, false},
		{"ordinary running pod is not a hook", `{"status":{"phase":"Running"}}`, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sigs := Inspect(Ref{Kind: "Pod", Name: "hook", Hook: tt.hook}, obj(t, tt.json), now, 0)
			if got := hasKind(sigs, verdict.SignalJobRunning); got != tt.busy {
				t.Errorf("job signal = %v, want %v", got, tt.busy)
			}
		})
	}
}

func TestRecentChange(t *testing.T) {
	stamp := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339) }
	withFields := func(entries ...string) string {
		return `{"metadata":{"managedFields":[` + strings.Join(entries, ",") + `]}}`
	}
	entry := func(manager string, ago time.Duration) string {
		return `{"manager":"` + manager + `","operation":"Update","time":"` + stamp(ago) + `"}`
	}
	tests := []struct {
		name    string
		json    string
		window  time.Duration
		want    bool
		manager string
	}{
		{"changed inside the window", withFields(entry("helm", 2*time.Minute)), 10 * time.Minute, true, "helm"},
		{"changed before the window", withFields(entry("helm", 20*time.Minute)), 10 * time.Minute, false, ""},
		{"newest entry decides", withFields(entry("helm", 3*time.Hour), entry("kube-controller-manager", time.Minute)), 10 * time.Minute, true, "kube-controller-manager"},
		{"any manager counts", withFields(entry("kubectl-edit", time.Minute)), 10 * time.Minute, true, "kubectl-edit"},
		{"zero window ignores modifications", withFields(entry("helm", 0)), 0, false, ""},
		{"creation time is the fallback", `{"metadata":{"creationTimestamp":"` + stamp(time.Minute) + `"}}`, 10 * time.Minute, true, ""},
		{"no timestamps at all", `{"metadata":{}}`, 10 * time.Minute, false, ""},
		{"unparsable timestamp is ignored", withFields(`{"manager":"helm","time":"yesterday"}`), 10 * time.Minute, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sigs := Inspect(Ref{Kind: "ConfigMap", Name: "cfg"}, obj(t, tt.json), now, tt.window)
			if got := hasKind(sigs, verdict.SignalRecentChange); got != tt.want {
				t.Fatalf("recent-change = %v, want %v (signals %v)", got, tt.want, sigs)
			}
			if tt.want && tt.manager != "" && !strings.Contains(sigs[0].Detail, "by "+tt.manager) {
				t.Errorf("detail %q does not name manager %q", sigs[0].Detail, tt.manager)
			}
		})
	}
}

func TestLatestModification(t *testing.T) {
	o := obj(t, `{"metadata":{"managedFields":[
		{"manager":"a","time":"2026-09-29T10:00:00Z"},
		{"manager":"b","time":"2026-09-29T11:00:00Z"},
		{"manager":"c"}]}}`)
	at, manager, ok := LatestModification(o)
	if !ok || manager != "b" || !at.Equal(time.Date(2026, 9, 29, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("LatestModification = %v %q %v", at, manager, ok)
	}
}

func TestSignalsCombine(t *testing.T) {
	o := obj(t, `{"metadata":{"generation":2,"managedFields":[{"manager":"helm","time":"`+now.Add(-time.Minute).Format(time.RFC3339)+`"}]},
		"spec":{"replicas":1},"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"availableReplicas":0}}`)
	got := kinds(Inspect(Ref{Kind: "Deployment", Name: "web"}, o, now, 10*time.Minute))
	if len(got) != 2 || got[0] != verdict.SignalRecentChange || got[1] != verdict.SignalRollout {
		t.Errorf("signals = %v, want recent-change then rollout", got)
	}
}

func TestIntAtNumericTypes(t *testing.T) {
	// The dynamic client decodes JSON numbers as int64, other decoders as float64 or json.Number.
	for name, v := range map[string]interface{}{
		"int": 3, "int32": int32(3), "int64": int64(3), "float64": float64(3), "json.Number": json.Number("3"),
	} {
		got, ok := intAt(map[string]interface{}{"n": v}, "n")
		if !ok || got != 3 {
			t.Errorf("%s: intAt = %d, %v", name, got, ok)
		}
	}
	if _, ok := intAt(map[string]interface{}{"n": "3"}, "n"); ok {
		t.Error("a string must not parse as a number")
	}
}

func TestWorkloadSignalsAreBoundedByHelmTimeout(t *testing.T) {
	sigs := Inspect(Ref{Kind: "Deployment", Name: "web"}, obj(t, `{"metadata":{"generation":2},"spec":{"replicas":1},"status":{"observedGeneration":1}}`), now, 0)
	if len(sigs) != 1 || !sigs[0].Bounded {
		t.Fatalf("a Helm-wait signal must be bounded by --helm-timeout: %+v", sigs)
	}
	recent := obj(t, `{"metadata":{"creationTimestamp":"`+now.Add(-time.Minute).Format(time.RFC3339)+`"}}`)
	sigs = Inspect(Ref{Kind: "ConfigMap", Name: "cfg"}, recent, now, 10*time.Minute)
	if len(sigs) != 1 || sigs[0].Bounded {
		t.Fatalf("a recent change is real activity and must not expire with the Helm timeout: %+v", sigs)
	}
}

func TestPodReadiness(t *testing.T) {
	tests := []struct {
		name string
		json string
		busy bool
	}{
		{"running but not ready", `{"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}]}}`, true},
		{"pending", `{"status":{"phase":"Pending"}}`, true},
		{"ready", `{"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sigs := Inspect(Ref{Kind: "Pod", Name: "p"}, obj(t, tt.json), now, 0)
			if got := hasKind(sigs, verdict.SignalNotReady); got != tt.busy {
				t.Errorf("not-ready signal = %v, want %v (%v)", got, tt.busy, sigs)
			}
		})
	}
}

func TestPersistentVolumeClaim(t *testing.T) {
	if sigs := Inspect(Ref{Kind: "PersistentVolumeClaim", Name: "data"}, obj(t, `{"status":{"phase":"Pending"}}`), now, 0); !hasKind(sigs, verdict.SignalNotReady) {
		t.Errorf("a Pending claim is something Helm --wait waits for: %v", sigs)
	}
	if sigs := Inspect(Ref{Kind: "PersistentVolumeClaim", Name: "data"}, obj(t, `{"status":{"phase":"Bound"}}`), now, 0); len(sigs) != 0 {
		t.Errorf("a Bound claim is ready: %v", sigs)
	}
}

func TestService(t *testing.T) {
	tests := []struct {
		name string
		json string
		busy bool
	}{
		{"load balancer without ingress", `{"spec":{"type":"LoadBalancer","clusterIP":"10.0.0.1"},"status":{"loadBalancer":{}}}`, true},
		{"load balancer with ingress", `{"spec":{"type":"LoadBalancer","clusterIP":"10.0.0.1"},"status":{"loadBalancer":{"ingress":[{"ip":"1.2.3.4"}]}}}`, false},
		{"load balancer with external IPs", `{"spec":{"type":"LoadBalancer","clusterIP":"10.0.0.1","externalIPs":["1.2.3.4"]},"status":{"loadBalancer":{}}}`, false},
		{"cluster IP not allocated", `{"spec":{"type":"ClusterIP"}}`, true},
		{"cluster IP", `{"spec":{"type":"ClusterIP","clusterIP":"10.0.0.1"}}`, false},
		{"headless", `{"spec":{"type":"ClusterIP","clusterIP":"None"}}`, false},
		{"external name", `{"spec":{"type":"ExternalName"}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sigs := Inspect(Ref{Kind: "Service", Name: "lb"}, obj(t, tt.json), now, 0)
			if got := hasKind(sigs, verdict.SignalNotReady); got != tt.busy {
				t.Errorf("not-ready signal = %v, want %v (%v)", got, tt.busy, sigs)
			}
		})
	}
}

func TestReplicaSetAndCRD(t *testing.T) {
	if sigs := Inspect(Ref{Kind: "ReplicaSet", Name: "rs"}, obj(t, `{"metadata":{"generation":2},"spec":{"replicas":2},"status":{"observedGeneration":2,"readyReplicas":1}}`), now, 0); !hasKind(sigs, verdict.SignalNotReady) {
		t.Errorf("ReplicaSet with 1 of 2 replicas ready: %v", sigs)
	}
	if sigs := Inspect(Ref{Kind: "ReplicaSet", Name: "rs"}, obj(t, `{"metadata":{"generation":2},"spec":{"replicas":2},"status":{"observedGeneration":2,"readyReplicas":2}}`), now, 0); len(sigs) != 0 {
		t.Errorf("ready ReplicaSet: %v", sigs)
	}
	crd := Ref{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: "foos.example.com"}
	if sigs := Inspect(crd, obj(t, `{"status":{"conditions":[{"type":"Established","status":"False"}]}}`), now, 0); !hasKind(sigs, verdict.SignalNotReady) {
		t.Errorf("a CRD that is not Established: %v", sigs)
	}
	if sigs := Inspect(crd, obj(t, `{"status":{"conditions":[{"type":"Established","status":"True"}]}}`), now, 0); len(sigs) != 0 {
		t.Errorf("an Established CRD: %v", sigs)
	}
}

func TestCustomResourceConditions(t *testing.T) {
	cr := Ref{APIVersion: "cert-manager.io/v1", Kind: "Certificate", Name: "tls"}
	if sigs := Inspect(cr, obj(t, `{"status":{"conditions":[{"type":"Ready","status":"False"}]}}`), now, 0); !hasKind(sigs, verdict.SignalNotReady) {
		t.Errorf("a custom resource reporting Ready=False: %v", sigs)
	}
	if sigs := Inspect(cr, obj(t, `{"status":{"conditions":[{"type":"Reconciling","status":"True"}]}}`), now, 0); !hasKind(sigs, verdict.SignalNotReady) {
		t.Errorf("a custom resource that is Reconciling: %v", sigs)
	}
	if sigs := Inspect(cr, obj(t, `{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`), now, 0); len(sigs) != 0 {
		t.Errorf("a ready custom resource: %v", sigs)
	}
	cm := Ref{APIVersion: "v1", Kind: "ConfigMap", Name: "cfg"}
	if sigs := Inspect(cm, obj(t, `{"status":{"conditions":[{"type":"Ready","status":"False"}]}}`), now, 0); len(sigs) != 0 {
		t.Errorf("built-in kinds without readiness rules are never waited on: %v", sigs)
	}
}
