package cluster

import (
	"context"
	"errors"
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
	got, err := k.fetch(ctx, liveness.Ref{APIVersion: "apps/v1beta1", Kind: "Deployment", Namespace: "prod", Name: "web"}, clock)
	if err != nil || len(got) != 1 {
		t.Fatalf("served version fallback: %v, %v", got, err)
	}

	// A kind the cluster has never heard of must fail the check, not read as "gone".
	_, err = k.fetch(ctx, liveness.Ref{APIVersion: "example.com/v1", Kind: "Widget", Namespace: "prod", Name: "w"}, clock)
	if err == nil || errors.Is(err, errGone) || !strings.Contains(err.Error(), "cannot be checked") {
		t.Errorf("err = %v: an unknown kind is an incomplete check", err)
	}

	// A missing object of a known kind is gone, as before.
	_, err = k.fetch(ctx, liveness.Ref{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "prod", Name: "nope"}, clock)
	if !errors.Is(err, errGone) {
		t.Errorf("err = %v, want errGone", err)
	}
}

func TestGeneratedNamesAreFoundByPrefixAndAge(t *testing.T) {
	start := clock
	k := testAccess(t,
		object("batch/v1", "Job", "prod", "migrate-x7k2", start.Add(time.Minute)),
		object("batch/v1", "Job", "prod", "migrate-old1", start.Add(-time.Hour)), // an earlier release's job
		object("batch/v1", "Job", "prod", "other-abcde", start.Add(time.Minute)),
	)
	got, err := k.fetch(context.Background(), liveness.Ref{APIVersion: "batch/v1", Kind: "Job", Namespace: "prod", GenerateName: "migrate-"}, start.Add(-generatedSlack))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].GetName() != "migrate-x7k2" {
		names := []string{}
		for _, o := range got {
			names = append(names, o.GetName())
		}
		t.Errorf("found %v, want only migrate-x7k2", names)
	}
}
