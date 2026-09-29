# helm-unstick

Recovers Helm releases stuck in `pending-install`, `pending-upgrade` or `pending-rollback`, and refuses to touch one that might still be running.

```console
$ helm upgrade --install web ./chart -n prod --wait
Error: UPGRADE FAILED: another operation (install/upgrade/rollback) is in progress
```

A CI job was killed mid-deploy, and every deploy since fails with that error. Two commands find out why and fix it (output from a real run on a kind cluster with Helm 3.22.0; `--older-than 0s` is there only because the demo release was only 27 seconds old, the default threshold is 10m):

```console
$ helm-unstick scan -n prod --older-than 0s
NAMESPACE  RELEASE  STATUS           REV  AGE  LAST-DEPLOYED  VERDICT  ACTION
prod       web      pending-upgrade  2    27s  1              stale    fix: roll back to revision 1

Run "helm-unstick explain RELEASE -n NAMESPACE" for the reasons and the recovery plan.
```

```console
$ helm-unstick fix web -n prod --older-than 0s --yes
Release:   web (namespace prod)
Status:    pending-upgrade, revision 2, last written 27s ago
Chart:     slowapp-0.1.0
Deployed:  revision 1 is the newest deployed revision
Verdict:   stale
  - the pending revision was written 27s ago, more than the 0s threshold
  - the recent-change window is 0s, so object modification times were not considered
  - no Job or hook Pod of the release is running
  - no Deployment, StatefulSet or DaemonSet of the release is mid-rollout

Plan:      Roll back to revision 1, the newest deployed revision.
  1. Roll back release "web" to revision 1. Helm records the result as revision 3.
  2. Mark any of the pending revisions (2) that the rollback leaves behind as failed, so the history stops showing an operation in progress.
Commands:
  helm-unstick fix web -n prod --older-than 0s
    # recover with helm-unstick
  helm rollback web 1 -n prod
    # the rollback step alone, by hand

Rolling back web to revision 1 ...
Rollback path: direct
Revision 2 was left pending-upgrade by the interrupted operation and is now marked failed.
Done. Release web is deployed as revision 3 (content of revision 1). The next helm upgrade can proceed.
```

If the operation might still be alive, `fix` says so, prints the reasons, and exits with code 3 without changing anything (`Refused: ...`).

## Why releases get stuck

Helm has no lock. A release counts as busy when the latest revision has a `pending-*` status, and Helm sets that status before it starts and clears it when it finishes. If the process dies in between (job timeout, Ctrl-C, an evicted runner pod), nothing clears it. Every later `helm upgrade` sees the pending revision and stops.

The usual workarounds are deleting the release secret by hand or running `helm rollback` from a blog post, both without checking that the first operation is really gone. Reports of the same problem:

- https://stackoverflow.com/questions/71599858
- https://stackoverflow.com/questions/65006907
- https://github.com/helm/helm/issues/8987
- https://github.com/helm/helm/issues/10599
- https://habr.com/ru/companies/kts/articles/723148/
- https://www.reddit.com/r/devops/comments/17tlqjl/

Helmwave has a `pending_release_strategy` option, but it only helps if you deploy with Helmwave. The `helm-set-status` plugin sets a release status by hand.

## How it decides it is safe

The age of a pending record does not prove the operation is dead: a slow but healthy `helm upgrade --wait` can run for an hour. So `helm-unstick` never acts on age alone. It looks at the release's objects in the cluster and gives one of three verdicts:

| Verdict | Meaning | `fix` |
| --- | --- | --- |
| `stale` | The pending record is older than `--older-than` (default 10m) and nothing shows activity. | acts |
| `possibly-running` | The record is younger than the threshold, or at least one sign of activity was found. | refuses, exit 3, no override |
| `unknown` | Some check could not be completed, for example a `Forbidden` on a resource. | refuses, exit 3, unless `--force-unknown` |

The signs of activity come from the objects in the manifest and the hooks of the pending revision:

- an object was modified inside the `--older-than` window (newest `managedFields` timestamp, any manager);
- a hook `Job` or hook `Pod` has not finished, or a `Job` of the release has neither completed nor failed;
- a `Deployment`, `StatefulSet` or `DaemonSet` is mid-rollout, judged the way `kubectl rollout status` judges it. A Deployment that hit its progress deadline is failed, not running.

The age is the later of `info.last_deployed` and the write time of the storage record (`createdAt` or `modifiedAt`). It is compared with the clock of the machine running `helm-unstick`, so a large skew against the cluster can shift the result. A timestamp in the future counts as "too young".

Right before acting, `fix` reads the history again. If the pending record changed since the verdict, it stops: a new operation may have started.

`--older-than 0s` switches the age rule and the modification window off. Rollouts and running hooks still block.

## Recovery matrix

| Latest revision | Deployed revision in history | What `fix` does |
| --- | --- | --- |
| `pending-upgrade` | yes | Roll back to the newest deployed revision. |
| `pending-rollback` | yes | Roll back to the newest deployed revision. |
| `pending-install` | not applicable | Print the plan only. Continue with `--first-install=mark-failed` or `--first-install=uninstall`. |
| `pending-upgrade`, `pending-rollback` | none | The same as an interrupted first install. |
| anything else | | Nothing to do. |

An interrupted first install has no earlier state to return to, so `helm-unstick` does not choose for you:

- `--first-install=mark-failed` sets the pending revision to `failed`. The resources the install created stay, and the next `helm upgrade --install` proceeds.
- `--first-install=uninstall` removes the release and its stored history, like `helm uninstall`. Objects with `helm.sh/resource-policy: keep` and volumes created from StatefulSet `volumeClaimTemplates` are not removed.

Without the flag `fix` prints both options with exact commands and exits with code 2.

### How the rollback runs

`fix` rolls back with the Helm SDK. Whether the SDK accepts a rollback while the latest revision is still `pending-*` was checked with e2e runs on kind:

| Helm CLI that left the release stuck | `pending-upgrade` | `pending-rollback` |
| --- | --- | --- |
| Helm 3.22.0 | direct rollback works | direct rollback works |
| Helm 4.3.0 | direct rollback works | direct rollback works |

So no `pending -> failed` step is needed first, and `--strategy mark-failed` (which does that step and then rolls back) works as well. `auto` keeps the fallback in case a future Helm refuses. Choose with `--strategy`:

| Strategy | Behavior |
| --- | --- |
| `auto` (default) | Try the rollback directly. If Helm refuses and the pending record is still untouched, mark it `failed` and try again. |
| `direct` | Try the rollback directly and report the error. |
| `mark-failed` | Mark the pending revision `failed`, then roll back. |

The output names the path that worked (`Rollback path: ...`). `make e2e` prints the path each run took.

After a successful rollback, any older revision that still says `pending-*` is set to `failed`, so `helm history` no longer shows an operation in progress. The rollback runs Helm's rollback hooks, as `helm rollback` does.

## What it does not do

- It never deletes release secrets.
- It never touches a release whose latest revision is not `pending-*`. A `failed` release is a different problem.
- It does not repair broken workloads. A rollback returns the release to the last deployed revision, and that is all.
- It cannot prove an operation is dead. It can only fail to find any sign of life, and it says which checks it ran.
- It does not need a `helm` binary. It embeds the Helm 3 SDK (`helm.sh/helm/v3`) and reads and writes the `sh.helm.release.v1` records that both Helm 3 and Helm 4 use. The e2e suite creates the stuck states with the Helm 4 CLI too and recovers them with this SDK, and the follow-up `helm upgrade` from Helm 4 succeeds. The v3 module is used because it already covers what is needed (history, storage update, rollback, uninstall) for both CLIs; the `helm.sh/helm/v4` SDK was not needed and not tried.

## Commands

```text
helm-unstick scan    [-n NAMESPACE | -A] [--older-than 10m] [-o table|json]
helm-unstick explain RELEASE [-n NAMESPACE] [--older-than 10m]
helm-unstick fix     RELEASE [-n NAMESPACE] [--dry-run] [--yes] [--older-than 10m]
                     [--force-unknown] [--first-install uninstall|mark-failed]
                     [--strategy auto|direct|mark-failed] [--wait] [--timeout 5m]
```

Common flags: `--kube-context`, `--kubeconfig` (`KUBECONFIG` works as in kubectl), `--driver secret|configmap` (default `$HELM_DRIVER`, then `secret`).

`scan` lists every release whose latest revision is pending, with the verdict and the recommended action. `-o json` gives an array for scripts. `explain` prints the reasons, the recovery plan with exact commands, and the revision history. `fix --dry-run` prints the plan and changes nothing. Without `--yes`, `fix` asks for confirmation and refuses to run when stdin is not a terminal.

| Exit code | Meaning |
| --- | --- |
| 0 | Done, or nothing is stuck. |
| 1 | Error. |
| 2 | `scan` found stuck releases, or `fix` left one stuck for you to decide. |
| 3 | `fix` refused: the operation may be running, or liveness is unknown. |

Permissions: reading the release records in the namespace, `get` on the kinds in the release manifest, and, for `fix`, whatever `helm rollback` or `helm uninstall` needs. A missing permission makes the verdict `unknown`.

## CI recipes

Run `fix` before the deploy and let it fail closed. `|| true` lets the pipeline continue when there is nothing to fix. If `fix` refused because the operation is alive, the following `helm upgrade` fails with the usual "another operation" error, which is the correct outcome.

### GitLab CI

`resource_group` runs the deploy jobs of one environment one at a time. When a job starts, the previous one has finished or died, so the age check is a formality there.

```yaml
deploy:
  stage: deploy
  resource_group: production
  script:
    - helm-unstick fix "$RELEASE" -n "$NS" --older-than 15m --yes || true
    - helm upgrade --install "$RELEASE" ./chart -n "$NS" --wait --timeout 10m
```

### GitHub Actions

`cancel-in-progress: true` kills a running deploy when a newer one starts, which is exactly how a release gets stuck. Queue the runs instead.

```yaml
concurrency:
  group: deploy-production
  cancel-in-progress: false

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Recover a stuck release
        run: helm-unstick fix "$RELEASE" -n "$NS" --older-than 15m --yes || true
      - name: Deploy
        run: helm upgrade --install "$RELEASE" ./chart -n "$NS" --wait --timeout 10m
```

## Install

Release binaries for Linux and macOS (amd64, arm64) are on the [releases page](https://github.com/DanilaZanin/helm-unstick/releases). Each archive holds `helm-unstick` and `kubectl-unstick`.

```console
$ go install github.com/DanilaZanin/helm-unstick/cmd/helm-unstick@latest
```

As a Helm plugin (the hook downloads the release binary and checks its checksum):

```console
$ helm plugin install https://github.com/DanilaZanin/helm-unstick
$ helm unstick scan -A
```

As a kubectl plugin, put `kubectl-unstick` on your `PATH`:

```console
$ kubectl unstick scan -A
```

With Krew, once the plugin is in the index: `kubectl krew install unstick`. The manifest template for the release bot is `.krew.yaml`.

Under `helm unstick`, Helm consumes `-n`, `--kube-context` and `--kubeconfig` itself and passes them on as `HELM_NAMESPACE`, `HELM_KUBECONTEXT` and `KUBECONFIG` (checked with Helm 4.3.0). `helm-unstick` reads all three, so the flags work the same way in every mode.

## Development

```console
$ make bootstrap   # first time only: resolves dependencies and writes go.sum
$ make build       # bin/helm-unstick and bin/kubectl-unstick
$ make test        # unit tests
$ make lint        # golangci-lint
$ make e2e         # kind cluster, four scenarios; HELM=/path/to/helm4 picks the Helm CLI
```

The e2e script (`test/e2e.sh`) uses the Helm CLI under test to start `helm upgrade --wait`, `helm install --wait` and `helm rollback --wait` against a chart with a slow readiness probe, kills the client with `kill -9`, and checks what `helm-unstick` does:

1. An interrupted upgrade is `stale`, `fix` restores the previous revision, and the next `helm upgrade` passes. It runs once per `--strategy` and records which path each one took.
2. A live slow upgrade is `possibly-running`, `fix` exits with 3, and `--force-unknown` does not change that.
3. An interrupted first install: `fix` alone prints the plan and changes nothing. Then `--first-install=mark-failed` lets `helm upgrade --install` pass, and `--first-install=uninstall` lets `helm install` pass.
4. An interrupted rollback returns to the last deployed revision.

CI runs the suite against the newest Helm 3 and Helm 4 (verified locally with 3.22.0 and 4.3.0).

Layout: `internal/model` (history and stuck detection), `internal/verdict` and `internal/liveness` (the liveness rules), `internal/plan` (recovery plans), `internal/app` (the commands, against a `Backend` interface), `internal/cluster` (the Helm SDK and client-go implementation).

## License

MIT. See [LICENSE](LICENSE).
