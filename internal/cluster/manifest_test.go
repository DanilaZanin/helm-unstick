package cluster

import (
	"reflect"
	"testing"

	"helm.sh/helm/v3/pkg/release"

	"github.com/DanilaZanin/helm-unstick/internal/liveness"
)

const sampleManifest = `---
# Source: web/templates/service.yaml
apiVersion: v1
kind: Service
metadata:
  name: web
---
# Source: web/templates/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: other
spec:
  replicas: 2
---
# nothing but a comment
---
apiVersion: batch/v1
kind: Job
metadata:
  generateName: job-
`

func TestParseManifest(t *testing.T) {
	got, err := parseManifest(sampleManifest, "prod", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []liveness.Ref{
		{APIVersion: "v1", Kind: "Service", Namespace: "prod", Name: "web"},
		{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "other", Name: "web"},
		// a name Kubernetes picks later must not vanish from the checks
		{APIVersion: "batch/v1", Kind: "Job", Namespace: "prod", GenerateName: "job-"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseManifest = %+v\nwant %+v", got, want)
	}
}

func TestParseManifestList(t *testing.T) {
	got, err := parseManifest(`apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: ConfigMap
    metadata:
      name: a
  - apiVersion: v1
    kind: ConfigMap
    metadata:
      name: b
`, "prod", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" || !got[0].Hook {
		t.Errorf("parseManifest(List) = %+v", got)
	}
}

func TestParseManifestEmptyAndInvalid(t *testing.T) {
	if got, err := parseManifest("", "prod", false); err != nil || len(got) != 0 {
		t.Errorf("empty manifest: %+v, %v", got, err)
	}
	if _, err := parseManifest("kind: [unclosed", "prod", false); err == nil {
		t.Error("invalid YAML must be an error, so the verdict becomes unknown")
	}
}

func TestManifestRefsIncludesHooksAndDedupes(t *testing.T) {
	rel := &release.Release{
		Version:  3,
		Manifest: sampleManifest,
		Hooks: []*release.Hook{
			{Name: "web-migrate", Manifest: "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: web-migrate\n"},
			nil,
			{Name: "again", Manifest: "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: web-migrate\n"},
		},
	}
	got, err := manifestRefs(rel, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("refs = %+v, want 3 manifest objects (one with a generated name) and 1 hook", got)
	}
	last := got[3]
	if last.Kind != "Job" || last.Name != "web-migrate" || !last.Hook || last.Namespace != "prod" {
		t.Errorf("hook ref = %+v", last)
	}
}
