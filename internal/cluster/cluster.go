// Package cluster implements app.Backend with the Helm 3 SDK and client-go.
//
// The Helm 3 SDK is used for both Helm generations: Helm 4 stores releases in the same
// sh.helm.release.v1 records, so the SDK reads what either CLI wrote.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"

	"github.com/DanilaZanin/helm-unstick/internal/model"
)

// Client talks to one cluster through the kubeconfig held by the Helm CLI settings.
type Client struct {
	settings *cli.EnvSettings
	driver   string

	kube *kubeAccess // created on first use
}

// New returns a Client. driverName is the Helm storage driver ("" means secret).
func New(settings *cli.EnvSettings, driverName string) *Client {
	return &Client{settings: settings, driver: driverName}
}

// Namespace is the namespace of the kube context unless the user chose another.
func (c *Client) Namespace() string { return c.settings.Namespace() }

// config opens the Helm storage for one namespace. An empty namespace covers all of them.
func (c *Client) config(namespace string) (*action.Configuration, error) {
	cfg := new(action.Configuration)
	quiet := func(string, ...interface{}) {}
	if err := cfg.Init(c.settings.RESTClientGetter(), namespace, c.driver, quiet); err != nil {
		return nil, fmt.Errorf("opening Helm storage (driver %q): %w", c.driver, err)
	}
	return cfg, nil
}

type releaseKey struct{ namespace, name string }

// ListPending returns the full history of every release that has a pending-* revision.
func (c *Client) ListPending(ctx context.Context, namespace string) ([]model.History, error) {
	cfg, err := c.config(namespace)
	if err != nil {
		return nil, err
	}
	seen := map[releaseKey]bool{}
	for _, status := range model.PendingStatuses {
		rels, err := cfg.Releases.Driver.Query(map[string]string{"owner": "helm", "status": string(status)})
		if err != nil {
			if errors.Is(err, driver.ErrReleaseNotFound) {
				continue
			}
			return nil, fmt.Errorf("querying releases with status %s: %w", status, err)
		}
		for _, rel := range rels {
			ns := rel.Namespace
			if ns == "" {
				ns = namespace
			}
			seen[releaseKey{ns, rel.Name}] = true
		}
	}
	keys := make([]releaseKey, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].namespace != keys[j].namespace {
			return keys[i].namespace < keys[j].namespace
		}
		return keys[i].name < keys[j].name
	})
	histories := make([]model.History, 0, len(keys))
	for _, k := range keys {
		h, err := c.History(ctx, k.namespace, k.name)
		if err != nil {
			if errors.Is(err, model.ErrNotFound) {
				continue // deleted between the query and the read
			}
			return nil, err
		}
		histories = append(histories, h)
	}
	return histories, nil
}

// History returns every stored revision of one release.
func (c *Client) History(ctx context.Context, namespace, name string) (model.History, error) {
	cfg, err := c.config(namespace)
	if err != nil {
		return model.History{}, err
	}
	rels, err := cfg.Releases.History(name)
	if err != nil {
		if errors.Is(err, driver.ErrReleaseNotFound) {
			return model.History{}, fmt.Errorf("release %q not found in namespace %q: %w", name, namespace, model.ErrNotFound)
		}
		return model.History{}, fmt.Errorf("reading history of %q: %w", name, err)
	}
	h := model.History{Namespace: namespace, Release: name}
	for _, rel := range rels {
		h.Revisions = append(h.Revisions, toRevision(rel))
	}
	// The storage record carries a write time of its own; it is only needed for the latest
	// revision, and only ever makes the release look more recently active.
	if latest, ok := h.Latest(); ok && latest.Status.IsPending() {
		for i := range h.Revisions {
			if h.Revisions[i].Number == latest.Number {
				h.Revisions[i].ModifiedAt = c.recordWriteTime(ctx, namespace, name, latest.Number)
			}
		}
	}
	return h, nil
}

func toRevision(rel *release.Release) model.Revision {
	r := model.Revision{Number: rel.Version}
	if rel.Info != nil {
		r.Status = model.Status(rel.Info.Status)
		r.Updated = rel.Info.LastDeployed.Time
		r.Description = rel.Info.Description
	}
	if rel.Chart != nil && rel.Chart.Metadata != nil {
		r.Chart = rel.Chart.Metadata.Name + "-" + rel.Chart.Metadata.Version
	}
	return r
}

// Rollback rolls the release back to a revision with the SDK.
func (c *Client) Rollback(_ context.Context, namespace, name string, revision int, opts model.ActionOptions) error {
	cfg, err := c.config(namespace)
	if err != nil {
		return err
	}
	rb := action.NewRollback(cfg)
	rb.Version = revision
	rb.Wait = opts.Wait
	rb.Timeout = opts.Timeout
	return rb.Run(name)
}

// MarkFailed sets one revision to failed and rewrites its storage record.
func (c *Client) MarkFailed(_ context.Context, namespace, name string, revision int, reason string) error {
	cfg, err := c.config(namespace)
	if err != nil {
		return err
	}
	rel, err := cfg.Releases.Get(name, revision)
	if err != nil {
		return fmt.Errorf("loading revision %d of %q: %w", revision, name, err)
	}
	if rel.Info == nil {
		rel.Info = &release.Info{}
	}
	rel.Info.Status = release.StatusFailed
	rel.Info.Description = reason
	return cfg.Releases.Update(rel)
}

// Uninstall removes the release together with its history.
func (c *Client) Uninstall(_ context.Context, namespace, name string, opts model.ActionOptions) error {
	cfg, err := c.config(namespace)
	if err != nil {
		return err
	}
	un := action.NewUninstall(cfg)
	un.Wait = opts.Wait
	un.Timeout = opts.Timeout
	_, err = un.Run(name)
	return err
}
