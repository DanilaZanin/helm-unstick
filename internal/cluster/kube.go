package cluster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/DanilaZanin/helm-unstick/internal/liveness"
	"github.com/DanilaZanin/helm-unstick/internal/model"
	"github.com/DanilaZanin/helm-unstick/internal/verdict"
)

const (
	// maxReportedErrors caps how many failed checks are listed individually.
	maxReportedErrors = 8
	// lookupTimeout bounds one object lookup, so a slow API server cannot hang a scan.
	lookupTimeout = 20 * time.Second
)

// errGone marks an object that does not exist, or whose kind the cluster does not know.
// Neither is a sign of activity, and neither is a failed check.
var errGone = errors.New("object does not exist")

type kubeAccess struct {
	dyn    dynamic.Interface
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
	mapper, err := getter.ToRESTMapper()
	if err != nil {
		return nil, fmt.Errorf("creating REST mapper: %w", err)
	}
	c.kube = &kubeAccess{dyn: dyn, mapper: mapper}
	return c.kube, nil
}

// Inspect looks at the live objects of the pending revision: everything in its manifest
// plus its hooks. Objects that cannot be read are reported as failed checks, which makes
// the verdict "unknown" rather than "stale".
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

	failures := map[string]bool{}
	for _, ref := range refs {
		lookup, cancel := context.WithTimeout(ctx, lookupTimeout)
		obj, err := kube.fetch(lookup, ref)
		cancel()
		switch {
		case err == nil:
			ev.Checked++
			ev.Signals = append(ev.Signals, liveness.Inspect(ref, obj.Object, now, window)...)
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

func (k *kubeAccess) fetch(ctx context.Context, ref liveness.Ref) (*unstructured.Unstructured, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid apiVersion %q: %w", ref.APIVersion, err)
	}
	mapping, err := k.mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: ref.Kind}, gv.Version)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return nil, errGone
		}
		return nil, err
	}
	var ri dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ri = k.dyn.Resource(mapping.Resource).Namespace(ref.Namespace)
	} else {
		ri = k.dyn.Resource(mapping.Resource)
	}
	obj, err := ri.Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errGone
		}
		return nil, err
	}
	return obj, nil
}

// recordWriteTime returns the latest write time of the storage record of one revision:
// Helm stamps createdAt on create and modifiedAt on every update. Zero when unknown.
func (c *Client) recordWriteTime(ctx context.Context, namespace, name string, revision int) time.Time {
	resource := "secrets"
	switch c.driver {
	case "configmap", "configmaps":
		resource = "configmaps"
	case "", "secret", "secrets":
	default:
		return time.Time{} // memory or sql storage has no Kubernetes record to read
	}
	kube, err := c.access()
	if err != nil {
		return time.Time{}
	}
	gvr := schema.GroupVersionResource{Version: "v1", Resource: resource}
	recordName := fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, revision)
	obj, err := kube.dyn.Resource(gvr).Namespace(namespace).Get(ctx, recordName, metav1.GetOptions{})
	if err != nil {
		return time.Time{}
	}
	latest := obj.GetCreationTimestamp().Time
	labels := obj.GetLabels()
	for _, label := range []string{"createdAt", "modifiedAt"} {
		secs, err := strconv.ParseInt(labels[label], 10, 64)
		if err != nil {
			continue
		}
		if t := time.Unix(secs, 0); t.After(latest) {
			latest = t
		}
	}
	return latest
}
