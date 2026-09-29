// Package app implements the scan, explain and fix commands on top of a Backend.
//
// Everything that talks to Helm or Kubernetes sits behind the Backend interface, so the
// command logic is tested with a fake and the real client lives in internal/cluster.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/DanilaZanin/helm-unstick/internal/model"
	"github.com/DanilaZanin/helm-unstick/internal/verdict"
)

// Exit codes.
const (
	ExitOK      = 0 // done, or nothing is stuck
	ExitError   = 1 // usage or runtime error
	ExitStuck   = 2 // scan found stuck releases, or fix left one stuck for the user to decide
	ExitRefused = 3 // fix refused: the operation may be running, or liveness is unknown
)

// Defaults.
const (
	DefaultOlderThan = 10 * time.Minute
	DefaultTimeout   = 5 * time.Minute
)

// Global is the cluster selection shared by every command.
type Global struct {
	Namespace   string
	KubeContext string
	KubeConfig  string
	Driver      string
}

// Backend is everything the commands need from Helm and Kubernetes.
type Backend interface {
	// Namespace is the namespace used when the user gave none.
	Namespace() string
	// ListPending returns the full history of every release that has a pending-* revision.
	// An empty namespace means all namespaces.
	ListPending(ctx context.Context, namespace string) ([]model.History, error)
	// History returns the full history of one release; a missing release wraps model.ErrNotFound.
	History(ctx context.Context, namespace, release string) (model.History, error)
	// Inspect looks at the live objects of the pending revision and reports signs of activity.
	// Checks that cannot be completed go into Evidence.Errors rather than the returned error.
	Inspect(ctx context.Context, s *model.Stuck, now time.Time, window time.Duration) (verdict.Evidence, error)
	// Rollback rolls the release back to revision through the Helm SDK.
	Rollback(ctx context.Context, namespace, release string, revision int, opts model.ActionOptions) error
	// MarkFailed sets the status of one revision to failed.
	MarkFailed(ctx context.Context, namespace, release string, revision int, reason string) error
	// Uninstall removes the release and, unless kept, its history.
	Uninstall(ctx context.Context, namespace, release string, opts model.ActionOptions) error
}

// Factory builds a Backend once the flags are known.
type Factory func(Global) (Backend, error)

// Env is the process environment, injected for tests.
type Env struct {
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	Now         func() time.Time
	Interactive bool   // stdin is a terminal
	Tool        string // how the user invokes the tool: helm-unstick, helm unstick, kubectl unstick
	Version     string
}

// Run executes one command line and returns the process exit code.
func Run(ctx context.Context, args []string, env Env, newBackend Factory) int {
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.Tool == "" {
		env.Tool = "helm-unstick"
	}
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usage(env.Tool))
		return ExitError
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "scan":
		return runScan(ctx, rest, env, newBackend)
	case "explain":
		return runExplain(ctx, rest, env, newBackend)
	case "fix":
		return runFix(ctx, rest, env, newBackend)
	case "version", "--version", "-version":
		fmt.Fprintf(env.Stdout, "%s %s\n", env.Tool, versionOrDev(env.Version))
		return ExitOK
	case "help", "-h", "--help", "-help":
		fmt.Fprint(env.Stdout, usage(env.Tool))
		return ExitOK
	}
	fmt.Fprintf(env.Stderr, "unknown command %q\n\n%s", cmd, usage(env.Tool))
	return ExitError
}

func versionOrDev(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

func usage(tool string) string {
	return fmt.Sprintf(`%[1]s finds Helm releases stuck in a pending-* state and recovers them safely.

Usage:
  %[1]s scan    [-n NAMESPACE | -A] [--older-than 10m] [-o table|json]
  %[1]s explain RELEASE [-n NAMESPACE] [--older-than 10m]
  %[1]s fix     RELEASE [-n NAMESPACE] [--dry-run] [--yes] [--older-than 10m]
                [--force-unknown] [--first-install uninstall|mark-failed]
                [--strategy auto|direct|mark-failed] [--wait] [--timeout 5m]
  %[1]s version

Common flags:
  -n, --namespace NS     namespace of the release (default: namespace of the kube context)
  --kube-context NAME    kubeconfig context (KUBECONFIG is honored as in kubectl)
  --kubeconfig PATH      kubeconfig file
  --driver DRIVER        Helm storage driver: secret or configmap (default: $HELM_DRIVER, then secret)

Exit codes:
  0  ok, or nothing is stuck
  1  error
  2  scan found stuck releases, or fix left one stuck for you to decide
  3  fix refused: the operation may still be running, or liveness could not be verified

fix acts only on releases with verdict "stale". Run "%[1]s explain RELEASE" to see why.
`, tool)
}

// commonFlags binds the flags shared by every command.
type commonFlags struct{ Global }

func (c *commonFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.Namespace, "n", "", "")
	fs.StringVar(&c.Namespace, "namespace", "", "")
	fs.StringVar(&c.KubeContext, "kube-context", "", "")
	fs.StringVar(&c.KubeConfig, "kubeconfig", "", "")
	fs.StringVar(&c.Driver, "driver", "", "")
}

func (c *commonFlags) validate() error {
	switch c.Driver {
	case "", "secret", "secrets", "configmap", "configmaps":
		return nil
	}
	return fmt.Errorf("unsupported --driver %q: use secret or configmap", c.Driver)
}

// durationFlag is a time.Duration flag that remembers how the user spelled it, so the
// commands printed in a plan repeat it verbatim.
type durationFlag struct {
	d   time.Duration
	raw string
}

func (f *durationFlag) String() string {
	if f == nil {
		return ""
	}
	return f.raw
}

func (f *durationFlag) Set(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if d < 0 {
		return errors.New("must not be negative")
	}
	f.d, f.raw = d, s
	return nil
}

func newDurationFlag(d time.Duration) *durationFlag { return &durationFlag{d: d, raw: d.String()} }

// newFlagSet returns a quiet FlagSet: the commands print their own errors and usage.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseInterspersed parses flags that may appear before or after positional arguments,
// so both "fix web -n prod" and "fix -n prod web" work.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// passthrough lists the explicitly set flags among names, spelled so they can be pasted
// into a command line.
func passthrough(fs *flag.FlagSet, names ...string) []string {
	var out []string
	fs.Visit(func(f *flag.Flag) {
		if slices.Contains(names, f.Name) {
			out = append(out, "--"+f.Name+" "+shellQuote(f.Value.String()))
		}
	})
	return out
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"$`\\!*?;&|<>()") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseFailure reports a flag error, or prints help when it was requested.
func parseFailure(err error, env Env, name, help string) int {
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(env.Stdout, help)
		return ExitOK
	}
	fmt.Fprintf(env.Stderr, "%s %s: %v\n", env.Tool, name, err)
	return ExitError
}

func fail(env Env, format string, args ...any) int {
	fmt.Fprintf(env.Stderr, "Error: "+format+"\n", args...)
	return ExitError
}
