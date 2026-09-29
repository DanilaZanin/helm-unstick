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
	"os"
	"slices"
	"strings"
	"time"

	"github.com/DanilaZanin/helm-unstick/internal/model"
	"github.com/DanilaZanin/helm-unstick/internal/plan"
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
	// DefaultHelmTimeout is Helm's own default for --timeout.
	DefaultHelmTimeout = 5 * time.Minute
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
	// An empty namespace means all namespaces. Releases whose records cannot be read are
	// listed as problems, never dropped silently.
	ListPending(ctx context.Context, namespace string) (model.Listing, error)
	// History returns the full history of one release; a missing release wraps model.ErrNotFound.
	History(ctx context.Context, namespace, release string) (model.History, error)
	// Inspect looks at the live objects of the pending revision and reports signs of activity.
	// Checks that cannot be completed go into Evidence.Errors rather than the returned error.
	Inspect(ctx context.Context, s *model.Stuck, now time.Time, window time.Duration) (verdict.Evidence, error)
	// Rollback rolls the release back to revision through the Helm SDK and returns the number
	// of the revision it created (0 when it created none), also when it fails afterwards.
	// A refusal because the release has a pending revision wraps model.ErrPending. from is the
	// head revision as inspected: if its storage record is no longer exactly that (same status
	// and Version), nothing is written and the error wraps model.ErrConflict.
	Rollback(ctx context.Context, namespace, release string, from model.Revision, revision int, opts model.ActionOptions) (int, error)
	// MarkFailed sets one revision to failed, but only if its storage record is still the one
	// in from (same status and Version). Otherwise it writes nothing and wraps model.ErrConflict.
	// It returns the revision as stored afterwards (failed, with its new Version).
	MarkFailed(ctx context.Context, namespace, release string, from model.Revision, reason string) (model.Revision, error)
	// Uninstall removes the release together with its history. from is guarded like the one of Rollback.
	Uninstall(ctx context.Context, namespace, release string, from model.Revision, opts model.ActionOptions) error
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
	Getenv      func(string) string // os.Getenv when nil
}

// Run executes one command line and returns the process exit code.
func Run(ctx context.Context, args []string, env Env, newBackend Factory) int {
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.Tool == "" {
		env.Tool = "helm-unstick"
	}
	if env.Getenv == nil {
		env.Getenv = os.Getenv
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
  %[1]s scan    [-n NAMESPACE | -A] [--older-than 10m] [--helm-timeout 5m] [-o table|json]
  %[1]s explain RELEASE [-n NAMESPACE] [--older-than 10m] [--helm-timeout 5m]
  %[1]s fix     RELEASE [-n NAMESPACE] [--dry-run] [--yes] [--older-than 10m] [--helm-timeout 5m]
                [--force-unknown] [--first-install uninstall|mark-failed] [--to-revision N]
                [--strategy claim|direct] [--wait] [--timeout 5m]
  %[1]s version

Common flags:
  -n, --namespace NS     namespace of the release (default: namespace of the kube context)
  --kube-context NAME    kubeconfig context (KUBECONFIG is honored as in kubectl)
  --kubeconfig PATH      kubeconfig file
  --driver DRIVER        Helm storage driver: secret or configmap (default: $HELM_DRIVER, then secret)

Exit codes:
  0  ok, or nothing is stuck
  1  error, or a scan that could not read every release
  2  scan found stuck releases, or fix left one stuck for you to decide
  3  fix refused: the operation may still be running, or liveness could not be verified

--helm-timeout: set it to the --timeout your deploys pass to helm (default 5m, Helm's own
default). Helm applies --timeout to the pre-hooks, the wait and the post-hooks separately,
so a live helm --wait may still be waiting on a not-ready Pod, volume, load balancer or
rollout until the record is older than 3 x that timeout plus a minute. An active Job or
a running hook Pod blocks fix whatever the age: wait for it, or delete it if it is orphaned.
A value smaller than the real --timeout can let fix roll back under a live helm.

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

// thresholds are the two time limits that turn evidence into a verdict.
type thresholds struct{ olderThan, helmTimeout time.Duration }

// bindThresholds binds --older-than and --helm-timeout.
func bindThresholds(fs *flag.FlagSet) (older, helm *durationFlag) {
	older = newDurationFlag(DefaultOlderThan)
	helm = newDurationFlag(DefaultHelmTimeout)
	fs.Var(older, "older-than", "")
	fs.Var(helm, "helm-timeout", "")
	return older, helm
}

// planContext builds the plan.Context that repeats the flags the user passed, so every
// printed command acts on the same cluster, storage and thresholds.
//
// Under "helm unstick" Helm consumes --kube-context and --kubeconfig itself and hands them
// over as HELM_KUBECONTEXT and KUBECONFIG, so the flags never reach this program. The printed
// commands would then act on the default cluster, and the environment is read back.
func planContext(env Env, fs *flag.FlagSet, g Global) plan.Context {
	pc := plan.Context{
		Tool:      env.Tool,
		Flags:     passthrough(fs, "older-than", "helm-timeout", "kube-context", "kubeconfig", "driver"),
		HelmFlags: passthrough(fs, "kube-context", "kubeconfig"),
		Driver:    g.Driver,
	}
	if env.Getenv("HELM_PLUGIN_DIR") == "" {
		return pc
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	add := func(flag string) {
		pc.Flags = append(pc.Flags, flag)
		pc.HelmFlags = append(pc.HelmFlags, flag)
	}
	if v := env.Getenv("HELM_KUBECONTEXT"); v != "" && !explicit["kube-context"] {
		add("--kube-context " + shellQuote(v))
	}
	if v := env.Getenv("KUBECONFIG"); v != "" && !explicit["kubeconfig"] {
		if strings.ContainsRune(v, os.PathListSeparator) {
			// a list of files is not a valid --kubeconfig value, it only works as the variable
			pc.EnvPrefix = append(pc.EnvPrefix, "KUBECONFIG="+shellQuote(v))
		} else {
			add("--kubeconfig " + shellQuote(v))
		}
	}
	return pc
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
	// Only characters that no shell treats specially stay bare; everything else is quoted.
	safe := s != ""
	for _, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alnum && !strings.ContainsRune("._/:@=+,-", r) {
			safe = false
			break
		}
	}
	if safe {
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
