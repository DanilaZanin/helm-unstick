package cluster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/DanilaZanin/helm-unstick/internal/liveness"
	"github.com/DanilaZanin/helm-unstick/internal/model"
	"github.com/DanilaZanin/helm-unstick/internal/verdict"
)

const (
	// maxReportedErrors caps how many failed checks are listed individually.
	maxReportedErrors = 8
	// lookupTimeout bounds one object lookup, so a slow API server cannot hang a scan.
	lookupTimeout = 20 * time.Second
	// generatedSlack widens the creation-time filter for objects with a generated name: the
	// record's timestamp comes from this machine's view of the operation, the object's from the API server.
	generatedSlack = 5 * time.Minute
)

// errGone marks an object that does not exist. It is not a sign of activity and not a failed check.
var errGone = errors.New("object does not exist")

type kubeAccess struct {
	dyn    dynamic.Interface
	kc     kubernetes.Interface
	mapper meta.RESTMapper
}

func (c *Client) access() (*kubeAccess, error) {
	if c.kube != nil {
		return c.kube, nil
	}
	getter := c.settings.RESTClientGetter()
	restCfg, err := getter.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("creating Kubernetes client: %w", err)
	}
	kc, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("creating Kubernetes client: %w", err)
	}
	mapper, err := getter.ToRESTMapper()
	if err != nil {
		return nil, fmt.Errorf("creating REST mapper: %w", err)
	}
	c.kube = &kubeAccess{dyn: dyn, kc: kc, mapper: mapper}
	return c.kube, nil
}

// Inspect looks at the live objects of the pending revision: everything in its manifest
// plus its hooks. Objects that cannot be read or found by name are reported as failed
// checks, which makes the verdict "unknown" rather than "stale".
func (c *Client) Inspect(ctx context.Context, s *model.Stuck, now time.Time, window time.Duration) (verdict.Evidence, error) {
	var ev verdict.Evidence
	cfg, err := c.config(s.Namespace)
	if err != nil {
		return ev, err
	}
	rel, err := cfg.Releases.Get(s.Release, s.Pending.Number)
	if err != nil {
		return ev, fmt.Errorf("loading revision %d of %q: %w", s.Pending.Number, s.Release, err)
	}
	refs, err := manifestRefs(rel, s.Namespace)
	if err != nil {
		return ev, err
	}
	kube, err := c.access()
	if err != nil {
		return ev, err
	}
	since := s.Pending.Updated.Add(-generatedSlack)

	failures := map[string]bool{}
	for _, ref := range refs {
		lookup, cancel := context.WithTimeout(ctx, lookupTimeout)
		objs, err := kube.fetch(lookup, ref, since)
		cancel()
		switch {
		case err == nil:
			for _, o := range objs {
				ev.Checked++
				found := ref
				found.Name = o.GetName()
				ev.Signals = append(ev.Signals, liveness.Inspect(found, o.Object, now, window)...)
			}
		case errors.Is(err, errGone):
			// Nothing to look at: the operation may not have reached this object.
		default:
			failures[fmt.Sprintf("%s: %v", ref, err)] = true
		}
	}
	ev.Errors = summarize(failures)
	return ev, nil
}

func summarize(failures map[string]bool) []string {
	all := make([]string, 0, len(failures))
	for msg := range failures {
		all = append(all, msg)
	}
	sort.Strings(all)
	if len(all) > maxReportedErrors {
		extra := len(all) - maxReportedErrors
		all = append(all[:maxReportedErrors], fmt.Sprintf("and %d more objects could not be read", extra))
	}
	return all
}

// mapping resolves the REST resource of a ref. A cluster that does not serve the manifest's
// apiVersion may still serve the kind under another version, and the object may exist there;
// a kind the cluster does not know at all is a failed check, never proof that the object is gone.
func (k *kubeAccess) mapping(ref liveness.Ref) (*meta.RESTMapping, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid apiVersion %q: %w", ref.APIVersion, err)
	}
	gk := schema.GroupKind{Group: gv.Group, Kind: ref.Kind}
	m, err := k.mapper.RESTMapping(gk, gv.Version)
	if err == nil || !meta.IsNoMatchError(err) {
		return m, err
	}
	if m, altErr := k.mapper.RESTMapping(gk); altErr == nil {
		return m, nil
	}
	return nil, fmt.Errorf("the cluster does not serve %s %s, so the object cannot be checked (CRD not installed, or an API version that was removed)", ref.APIVersion, ref.Kind)
}

// fetch returns the live objects behind a ref: one for a named object, every object created
// since `since` whose name starts with the generateName prefix otherwise.
func (k *kubeAccess) fetch(ctx context.Context, ref liveness.Ref, since time.Time) ([]*unstructured.Unstructured, error) {
	mapping, err := k.mapping(ref)
	if err != nil {
		return nil, err
	}
	var ri dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ri = k.dyn.Resource(mapping.Resource).Namespace(ref.Namespace)
	} else {
		ri = k.dyn.Resource(mapping.Resource)
	}
	if ref.Name == "" {
		list, err := ri.List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing objects with generated names: %w", err)
		}
		var out []*unstructured.Unstructured
		for i := range list.Items {
			o := &list.Items[i]
			if strings.HasPrefix(o.GetName(), ref.GenerateName) && !o.GetCreationTimestamp().Time.Before(since) {
				out = append(out, o)
			}
		}
		return out, nil
	}
	obj, err := ri.Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errGone
		}
		return nil, err
	}
	return []*unstructured.Unstructured{obj}, nil
}
