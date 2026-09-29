package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/DanilaZanin/helm-unstick/internal/model"
)

var clock = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// releaseDoc is a stored release as the Helm SDK would decode it, plus a field this program
// knows nothing about.
func releaseDoc(t *testing.T, status string) []byte {
	t.Helper()
	enc, err := encodeRaw(map[string]any{
		"name": "web", "version": 3, "namespace": "prod",
		"info":   map[string]any{"status": status, "description": "Upgrade in progress"},
		"future": map[string]any{"big": 12345678901234567},
	})
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func secretRecord(t *testing.T, version, status string) *corev1.Secret {
	t.Helper()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sh.helm.release.v1.web.v3", Namespace: "prod", ResourceVersion: version,
			Labels:      map[string]string{"owner": "helm", "name": "web", "version": "3", "status": status, "createdAt": "100", "team": "payments"},
			Annotations: map[string]string{"keep": "me"},
		},
		Type: "helm.sh/release.v1",
		Data: map[string][]byte{"release": releaseDoc(t, status)},
	}
}

func pending(version string) model.Revision {
	return model.Revision{Number: 3, Status: model.StatusPendingUpgrade, Version: version}
}

func TestMarkRecordFailedRewritesOnlyTheStatus(t *testing.T) {
	kc := fake.NewSimpleClientset(secretRecord(t, "42", "pending-upgrade"))
	if _, err := markRecordFailed(context.Background(), kc, "", "prod", "web", pending("42"), "interrupted", clock); err != nil {
		t.Fatal(err)
	}
	got, _ := kc.CoreV1().Secrets("prod").Get(context.Background(), "sh.helm.release.v1.web.v3", metav1.GetOptions{})
	if got.Labels["status"] != "failed" || got.Labels["team"] != "payments" || got.Labels["createdAt"] != "100" || got.Labels["modifiedAt"] != "1790683200" {
		t.Errorf("labels = %v", got.Labels)
	}
	if got.Annotations["keep"] != "me" || got.Type != "helm.sh/release.v1" {
		t.Errorf("the rewrite lost annotations or the type: %+v", got.ObjectMeta)
	}
	doc, err := decodeRaw(got.Data["release"])
	if err != nil {
		t.Fatal(err)
	}
	info := doc["info"].(map[string]any)
	if info["status"] != "failed" || info["description"] != "interrupted" {
		t.Errorf("info = %v", info)
	}
	if doc["future"].(map[string]any)["big"].(interface{ String() string }).String() != "12345678901234567" {
		t.Errorf("a field this program does not know was damaged: %v", doc["future"])
	}
}

func TestMarkRecordFailedNeverOverwritesARecordThatMoved(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		rec  *corev1.Secret
		from model.Revision
		want error // nil = any error except a conflict
	}{
		{"helm finished: the record is deployed now", secretRecord(t, "43", "deployed"), pending("42"), model.ErrConflict},
		{"helm finished and the version matches by accident", secretRecord(t, "42", "deployed"), pending("42"), model.ErrConflict},
		{"somebody wrote to the pending record", secretRecord(t, "43", "pending-upgrade"), pending("42"), model.ErrConflict},
		{"no version to guard the write", secretRecord(t, "42", "pending-upgrade"), pending(""), nil},
		{"the caller passes a non-pending revision", secretRecord(t, "42", "deployed"), model.Revision{Number: 3, Status: model.StatusDeployed, Version: "42"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc := fake.NewSimpleClientset(tt.rec.DeepCopy())
			_, err := markRecordFailed(ctx, kc, "", "prod", "web", tt.from, "x", clock)
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			got, _ := kc.CoreV1().Secrets("prod").Get(ctx, tt.rec.Name, metav1.GetOptions{})
			if got.Labels["status"] != tt.rec.Labels["status"] || string(got.Data["release"]) != string(tt.rec.Data["release"]) {
				t.Errorf("the record was rewritten: labels %v", got.Labels)
			}
			for _, a := range kc.Actions() {
				if a.GetVerb() == "update" {
					t.Errorf("no update may be sent: %v", a)
				}
			}
		})
	}
}

func TestMarkRecordFailedSendsTheInspectedResourceVersion(t *testing.T) {
	kc := fake.NewSimpleClientset(secretRecord(t, "42", "pending-upgrade"))
	var sent string
	kc.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sent = a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret).ResourceVersion
		// the API server found a newer version between our read and our write
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "sh.helm.release.v1.web.v3", errors.New("the object has been modified"))
	})
	_, err := markRecordFailed(context.Background(), kc, "", "prod", "web", pending("42"), "x", clock)
	if !errors.Is(err, model.ErrConflict) {
		t.Errorf("a rejected update must surface as a conflict: %v", err)
	}
	if sent != "42" {
		t.Errorf("the update must carry the inspected resourceVersion so the server can reject it, sent %q", sent)
	}
}

func TestMarkRecordFailedConfigMapDriver(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.web.v3", Namespace: "prod", ResourceVersion: "7",
			Labels: map[string]string{"owner": "helm", "name": "web", "version": "3", "status": "pending-upgrade"}},
		Data: map[string]string{"release": string(releaseDoc(t, "pending-upgrade"))},
	}
	kc := fake.NewSimpleClientset(cm)
	if _, err := markRecordFailed(context.Background(), kc, "configmap", "prod", "web", pending("7"), "x", clock); err != nil {
		t.Fatal(err)
	}
	got, _ := kc.CoreV1().ConfigMaps("prod").Get(context.Background(), cm.Name, metav1.GetOptions{})
	doc, _ := decodeRaw([]byte(got.Data["release"]))
	if got.Labels["status"] != "failed" || doc["info"].(map[string]any)["status"] != "failed" {
		t.Errorf("labels %v, info %v", got.Labels, doc["info"])
	}
}

func TestPendingReleasesAndUndecodableRecords(t *testing.T) {
	ctx := context.Background()
	other := secretRecord(t, "9", "deployed")
	other.Name, other.Namespace, other.Labels["name"] = "sh.helm.release.v1.api.v1", "dev", "api"
	kc := fake.NewSimpleClientset(secretRecord(t, "42", "pending-upgrade"), other)
	keys, err := pendingReleases(ctx, kc, "", "")
	if err != nil || len(keys) != 1 || keys[0] != (releaseKey{"prod", "web"}) {
		t.Fatalf("keys = %v, err = %v", keys, err)
	}

}

func TestListRecordsSurfacesAccessErrors(t *testing.T) {
	kc := fake.NewSimpleClientset()
	kc.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("no"))
	})
	if _, err := listRecords(context.Background(), kc, "", "prod", "owner=helm"); !apierrors.IsForbidden(err) {
		t.Errorf("err = %v: an unreadable namespace must be an error, never an empty listing", err)
	}
}

func TestWrittenTakesTheLatestTimestamp(t *testing.T) {
	created := time.Unix(100, 0)
	got := written(created, map[string]string{"createdAt": "50", "modifiedAt": "300"})
	if !got.Equal(time.Unix(300, 0)) {
		t.Errorf("written = %v", got)
	}
}
