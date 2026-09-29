package cluster

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/DanilaZanin/helm-unstick/internal/liveness"
)

func object(apiVersion, kind, ns, name string, created time.Time) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind}}
	o.SetNamespace(ns)
	o.SetName(name)
	o.SetCreationTimestamp(metav1.NewTime(created))
	return o
}

func testAccess(t *testing.T, objs ...runtime.Object) *kubeAccess {
	t.Helper()
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "apps", Version: "v1"}, {Group: "batch", Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}, meta.RESTScopeNamespace)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		{Group: "apps", Version: "v1", Resource: "deployments"}: "DeploymentList",
		{Group: "batch", Version: "v1", Resource: "jobs"}:       "JobList",
	}, objs...)
	return &kubeAccess{dyn: dyn, mapper: mapper}
}

func TestUnservedAPIVersionIsNotProofThatTheObjectIsGone(t *testing.T) {
	k := testAccess(t, object("apps/v1", "Deployment", "prod", "web", clock))
	ctx := context.Background()

	// The manifest says apps/v1beta1, the cluster serves apps/v1: the object is found there.
	got, _, err := k.fetch(ctx, liveness.Ref{APIVersion: "apps/v1beta1", Kind: "Deployment", Namespace: "prod", Name: "web"}, clock, webOwner)
	if err != nil || len(got) != 1 {
		t.Fatalf("served version fallback: %v, %v", got, err)
	}

	// A kind the cluster has never heard of must fail the check, not read as "gone".
	_, _, err = k.fetch(ctx, liveness.Ref{APIVersion: "example.com/v1", Kind: "Widget", Namespace: "prod", Name: "w"}, clock, webOwner)
	if err == nil || errors.Is(err, errGone) || !strings.Contains(err.Error(), "cannot be checked") {
		t.Errorf("err = %v: an unknown kind is an incomplete check", err)
	}

	// A missing object of a known kind is gone, as before.
	_, _, err = k.fetch(ctx, liveness.Ref{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "prod", Name: "nope"}, clock, webOwner)
	if !errors.Is(err, errGone) {
		t.Errorf("err = %v, want errGone", err)
	}
}

var webOwner = owner{release: "web", namespace: "prod"}

func withMeta(o *unstructured.Unstructured, labels, annotations map[string]string) *unstructured.Unstructured {
	o.SetLabels(labels)
	o.SetAnnotations(annotations)
	return o
}

var helmOwned = map[string]string{"meta.helm.sh/release-name": "web", "meta.helm.sh/release-namespace": "prod"}

func names(objs []*unstructured.Unstructured) []string {
	out := []string{}
	for _, o := range objs {
		out = append(out, o.GetName())
	}
	return out
}

func TestGeneratedNamesAreFoundByPrefixAndAge(t *testing.T) {
	start := clock
	k := testAccess(t,
		withMeta(object("batch/v1", "Job", "prod", "migrate-x7k2", start.Add(time.Minute)), nil, helmOwned),
		withMeta(object("batch/v1", "Job", "prod", "migrate-old1", start.Add(-time.Hour)), nil, helmOwned), // an earlier release's job
		withMeta(object("batch/v1", "Job", "prod", "other-abcde", start.Add(time.Minute)), nil, helmOwned),
	)
	got, unproven, err := k.fetch(context.Background(), liveness.Ref{APIVersion: "batch/v1", Kind: "Job", Namespace: "prod", GenerateName: "migrate-"}, start.Add(-generatedSlack), webOwner)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].GetName() != "migrate-x7k2" || len(unproven) != 0 {
		t.Errorf("found %v (unproven %v), want only migrate-x7k2", names(got), unproven)
	}
}

func TestGeneratedNamesNeedProofOfOwnership(t *testing.T) {
	start := clock
	ref := liveness.Ref{APIVersion: "batch/v1", Kind: "Job", Namespace: "prod", GenerateName: "migrate-", Hook: true, Labels: "app=migrate,release=web"}
	fresh := start.Add(time.Minute)
	otherRelease := map[string]string{"meta.helm.sh/release-name": "api", "meta.helm.sh/release-namespace": "prod"}
	k := testAccess(t,
		withMeta(object("batch/v1", "Job", "prod", "migrate-own01", fresh), nil, helmOwned),
		withMeta(object("batch/v1", "Job", "prod", "migrate-inst02", fresh), map[string]string{"app.kubernetes.io/instance": "web"}, nil),
		withMeta(object("batch/v1", "Job", "prod", "migrate-rndr03", fresh), map[string]string{"release": "web"}, nil),
		withMeta(object("batch/v1", "Job", "prod", "migrate-foreign1", fresh), nil, otherRelease),
		withMeta(object("batch/v1", "Job", "prod", "migrate-foreign2", fresh), map[string]string{"app.kubernetes.io/instance": "api"}, nil),
		withMeta(object("batch/v1", "Job", "prod", "migrate-samens-other", fresh), nil, map[string]string{"meta.helm.sh/release-name": "web", "meta.helm.sh/release-namespace": "dev"}),
		object("batch/v1", "Job", "prod", "migrate-bare04", fresh),
	)
	got, unproven, err := k.fetch(context.Background(), ref, start.Add(-generatedSlack), webOwner)
	if err != nil {
		t.Fatal(err)
	}
	want := "migrate-inst02 migrate-own01 migrate-rndr03"
	if strings.Join(sorted(names(got)), " ") != want {
		t.Errorf("owned = %v, want %s: a foreign release's object must never be inspected", names(got), want)
	}
	if len(unproven) != 1 || unproven[0] != "migrate-bare04" {
		t.Errorf("unproven = %v: an object with no ownership metadata cannot be included, and must not be silently dropped either", unproven)
	}
}

func sorted(s []string) []string {
	sort.Strings(s)
	return s
}
