// Command helm-unstick finds Helm releases stuck in pending-install, pending-upgrade or
// pending-rollback and recovers them, but only when nothing shows the operation is alive.
//
// The same binary works as a standalone tool, as "kubectl unstick" (installed as
// kubectl-unstick) and as "helm unstick" (a Helm plugin).
package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"helm.sh/helm/v3/pkg/cli"

	"github.com/DanilaZanin/helm-unstick/internal/app"
	"github.com/DanilaZanin/helm-unstick/internal/cluster"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

var _ app.Backend = (*cluster.Client)(nil)

func main() { os.Exit(run()) }

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	env := app.Env{
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Interactive: isTerminal(os.Stdin),
		Tool:        invocationName(),
		Version:     buildVersion(),
	}
	return app.Run(ctx, os.Args[1:], env, newBackend)
}

// newBackend applies the flags on top of the Helm environment (HELM_NAMESPACE,
// HELM_KUBECONTEXT, KUBECONFIG), so kubectl and Helm users get the behavior they expect.
func newBackend(g app.Global) (app.Backend, error) {
	settings := cli.New()
	if g.KubeConfig != "" {
		settings.KubeConfig = g.KubeConfig
	}
	if g.KubeContext != "" {
		settings.KubeContext = g.KubeContext
	}
	if g.Namespace != "" {
		settings.SetNamespace(g.Namespace)
	}
	driver := g.Driver
	if driver == "" {
		driver = os.Getenv("HELM_DRIVER")
	}
	return cluster.New(settings, driver), nil
}

// invocationName is how the user called the tool, so printed commands can be pasted.
func invocationName() string {
	if os.Getenv("HELM_PLUGIN_DIR") != "" {
		return "helm unstick"
	}
	base := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if rest, ok := strings.CutPrefix(base, "kubectl-"); ok {
		return "kubectl " + rest
	}
	return base
}

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
