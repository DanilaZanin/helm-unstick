package cluster

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"helm.sh/helm/v3/pkg/release"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/DanilaZanin/helm-unstick/internal/liveness"
)

// manifestRefs lists the objects of a release revision: the rendered manifest and the hooks.
func manifestRefs(rel *release.Release, defaultNamespace string) ([]liveness.Ref, error) {
	refs, err := parseManifest(rel.Manifest, defaultNamespace, false)
	if err != nil {
		return nil, fmt.Errorf("parsing the manifest of revision %d: %w", rel.Version, err)
	}
	for _, hook := range rel.Hooks {
		if hook == nil {
			continue
		}
		hookRefs, err := parseManifest(hook.Manifest, defaultNamespace, true)
		if err != nil {
			return nil, fmt.Errorf("parsing hook %q of revision %d: %w", hook.Name, rel.Version, err)
		}
		refs = append(refs, hookRefs...)
	}
	return dedupe(refs), nil
}

func dedupe(refs []liveness.Ref) []liveness.Ref {
	seen := map[liveness.Ref]bool{}
	out := make([]liveness.Ref, 0, len(refs))
	for _, r := range refs {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// parseManifest extracts kind, name and namespace from every document of a YAML stream.
// A document with generateName instead of name becomes a Ref with GenerateName set.
func parseManifest(manifest, defaultNamespace string, hook bool) ([]liveness.Ref, error) {
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	var refs []liveness.Ref
	for {
		var doc map[string]interface{}
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return refs, nil
			}
			return nil, err
		}
		refs = appendRefs(refs, doc, defaultNamespace, hook)
	}
}

func appendRefs(refs []liveness.Ref, doc map[string]interface{}, defaultNamespace string, hook bool) []liveness.Ref {
	if len(doc) == 0 {
		return refs
	}
	if items, ok := doc["items"].([]interface{}); ok { // kind: List and friends
		for _, item := range items {
			if m, ok := item.(map[string]interface{}); ok {
				refs = appendRefs(refs, m, defaultNamespace, hook)
			}
		}
		return refs
	}
	apiVersion, _ := doc["apiVersion"].(string)
	kind, _ := doc["kind"].(string)
	metadata, _ := doc["metadata"].(map[string]interface{})
	name, _ := metadata["name"].(string)
	generateName, _ := metadata["generateName"].(string)
	namespace, _ := metadata["namespace"].(string)
	if apiVersion == "" || kind == "" || (name == "" && generateName == "") {
		return refs
	}
	if namespace == "" {
		namespace = defaultNamespace
	}
	if name != "" {
		generateName = ""
	}
	return append(refs, liveness.Ref{APIVersion: apiVersion, Kind: kind, Namespace: namespace, Name: name, GenerateName: generateName, Hook: hook})
}
