// Package cluster implements app.Backend with the Helm 3 SDK and client-go.
//
// The Helm 3 SDK is used for both Helm generations: Helm 4 stores releases in the same
// sh.helm.release.v1 records, so the SDK reads what either CLI wrote.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"

	"github.com/DanilaZanin/helm-unstick/internal/model"
)

// Client talks to one cluster through the kubeconfig held by the Helm CLI settings.
type Client struct {
	settings *cli.EnvSettings
	driver   string

	kube   *kubeAccess // created on first use
	stderr io.Writer   // where the interrupt notice goes
}

// New returns a Client. driverName is the Helm storage driver ("" means secret).
func New(settings *cli.EnvSettings, driverName string) *Client {
	return &Client{settings: settings, driver: driverName, stderr: os.Stderr}
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

// checkDriver rejects storage that has no Kubernetes records to read and guard.
func (c *Client) checkDriver() error {
	switch c.driver {
	case "", "secret", "secrets", "configmap", "configmaps":
		return nil
	}
	return fmt.Errorf("unsupported storage driver %q: only secret and configmap can be inspected safely", c.driver)
}

// ListPending returns the full history of every release that has a pending-* revision.
// A release whose records cannot be read becomes a Problem instead of disappearing.
func (c *Client) ListPending(ctx context.Context, namespace string) (model.Listing, error) {
	var out model.Listing
	if err := c.checkDriver(); err != nil {
		return out, err
	}
	kube, err := c.access()
	if err != nil {
		return out, err
	}
	keys, err := pendingReleases(ctx, kube.kc, c.driver, namespace)
	if err != nil {
		return out, fmt.Errorf("listing release records: %w", err)
	}
	for _, k := range keys {
		h, err := c.History(ctx, k.namespace, k.name)
		switch {
		case err == nil:
			out.Histories = append(out.Histories, h)
		case errors.Is(err, model.ErrNotFound):
			// deleted between the listing and the read
		default:
			out.Problems = append(out.Problems, model.Problem{Namespace: k.namespace, Release: k.name, Err: err.Error()})
		}
	}
	return out, nil
}

// History returns every stored revision of one release, built from a single snapshot of its
// storage records. Records that cannot be decoded, or that contradict their own labels, make
// it fail: a history with a hidden or doubtful revision is not something to act on.
func (c *Client) History(ctx context.Context, namespace, name string) (model.History, error) {
	if err := ctx.Err(); err != nil {
		return model.History{}, err
	}
	if err := c.checkDriver(); err != nil {
		return model.History{}, err
	}
	kube, err := c.access()
	if err != nil {
		return model.History{}, err
	}
	return readHistory(ctx, kube.kc, c.driver, namespace, name)
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

// recorder remembers which revisions a Helm action stored, so the caller learns the number of
// the revision its own rollback created instead of guessing from the latest one.
type recorder struct {
	driver.Driver
	mu      sync.Mutex
	created []int
}

func (r *recorder) Create(key string, rls *release.Release) error {
	err := r.Driver.Create(key, rls)
	if err == nil {
		r.mu.Lock()
		r.created = append(r.created, rls.Version)
		r.mu.Unlock()
	}
	return err
}

func (r *recorder) last() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.created) == 0 {
		return 0
	}
	return r.created[len(r.created)-1]
}

// runCtx runs a Helm SDK call, which takes no context and cannot be stopped half-way. The
// first interrupt therefore does not abandon it: an abandoned rollback would go on writing in
// a goroutine while the command reported an error. The call is allowed to finish, the user is
// told so, and a second Ctrl-C (the signal handler is gone by then) kills the process.
func (c *Client) runCtx(ctx context.Context, what string, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		fmt.Fprintf(c.stderr, "\nInterrupt received: finishing the current write (%s), press Ctrl-C again to abort. Aborting can leave a half-written release; check helm history afterwards.\n", what)
		return <-done
	}
}

// Rollback rolls the release back to a revision with the SDK and returns the revision it created.
//
// from is the head revision as it was inspected. Immediately before the SDK call its storage
// record is read again and must have the same status and resourceVersion, otherwise nothing is
// written. The SDK takes no precondition, so a small window remains between that read and the
// SDK's own first write: a helm that starts inside it is not seen.
func (c *Client) Rollback(ctx context.Context, namespace, name string, from model.Revision, revision int, opts model.ActionOptions) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := c.guard(ctx, namespace, name, from); err != nil {
		return 0, err
	}
	cfg, err := c.config(namespace)
	if err != nil {
		return 0, err
	}
	rec := &recorder{Driver: cfg.Releases.Driver}
	cfg.Releases = storage.Init(rec)
	rb := action.NewRollback(cfg)
	rb.Version = revision
	rb.Wait = opts.Wait
	rb.Timeout = opts.Timeout
	err = c.runCtx(ctx, "rollback", func() error { return rb.Run(name) })
	if err != nil {
		// Helm's refusal text is not an exported error value.
		if msg := err.Error(); strings.Contains(msg, "another operation") && strings.Contains(msg, "in progress") {
			err = fmt.Errorf("%w: %s", model.ErrPending, msg)
		}
	}
	return rec.last(), err
}

// MarkFailed sets one pending revision to failed, conditional on its storage record being
// exactly the one that was inspected.
// It returns the revision as it is stored afterwards (failed, with its new record version).
func (c *Client) MarkFailed(ctx context.Context, namespace, name string, from model.Revision, reason string) (model.Revision, error) {
	if err := ctx.Err(); err != nil {
		return model.Revision{}, err
	}
	if err := c.checkDriver(); err != nil {
		return model.Revision{}, err
	}
	kube, err := c.access()
	if err != nil {
		return model.Revision{}, err
	}
	return markRecordFailed(ctx, kube.kc, c.driver, namespace, name, from, reason, time.Now())
}

// guard re-reads the record of from right before an SDK write.
func (c *Client) guard(ctx context.Context, namespace, name string, from model.Revision) error {
	if err := c.checkDriver(); err != nil {
		return err
	}
	kube, err := c.access()
	if err != nil {
		return err
	}
	return guardRecord(ctx, kube.kc, c.driver, namespace, name, from)
}

// Uninstall removes the release together with its history. from is guarded like the one of
// Rollback.
func (c *Client) Uninstall(ctx context.Context, namespace, name string, from model.Revision, opts model.ActionOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.guard(ctx, namespace, name, from); err != nil {
		return err
	}
	cfg, err := c.config(namespace)
	if err != nil {
		return err
	}
	un := action.NewUninstall(cfg)
	un.Wait = opts.Wait
	un.Timeout = opts.Timeout
	return c.runCtx(ctx, "uninstall", func() error { _, err := un.Run(name); return err })
}
