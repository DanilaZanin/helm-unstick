# helm-unstick

Recovers Helm releases stuck in `pending-install`, `pending-upgrade` or `pending-rollback`, and refuses to touch one that might still be running.

```console
$ helm upgrade --install web ./chart -n prod --wait
Error: UPGRADE FAILED: another operation (install/upgrade/rollback) is in progress
```

A CI job was killed mid-deploy, and every deploy since fails with that error. Two commands find out why and fix it. The output below is from a real run on a kind cluster with Helm 3.22.0. The demo release is seconds old, so the runs pass `--older-than 0s` and `--helm-timeout 5s`; the defaults are 10m and 5m.

First the helm process is still alive: it waits with `--wait`, its Deployment is past `progressDeadlineSeconds`, and a LoadBalancer Service has no address. Helm 3 keeps waiting in that state, so `fix` refuses:

```console
$ helm-unstick scan -n prod --older-than 0s
NAMESPACE  RELEASE  STATUS           REV  AGE  LAST-DEPLOYED  VERDICT           ACTION
prod       web      pending-upgrade  2    12s  1              possibly-running  wait: the operation may still be running

Run "helm-unstick explain RELEASE -n NAMESPACE" for the reasons and the recovery plan.
exit=2

$ helm-unstick fix web -n prod --older-than 0s --yes
Release:   web (namespace prod)
Status:    pending-upgrade, revision 2, last written 12s ago
Chart:     slowapp-0.1.0
Deployed:  revision 1 is the newest deployed revision
Verdict:   possibly-running
  - Service/web-lb: helm --wait may still be waiting: load balancer has no ingress address yet [a live helm may still be waiting: the record is younger than --helm-timeout 5m plus 1m grace; set --helm-timeout to the --timeout of your deploy]
  - Deployment/web: rollout in progress: 1 old replicas still terminating (progress deadline exceeded, but Helm 3 --wait keeps waiting) [a live helm may still be waiting: the record is younger than --helm-timeout 5m plus 1m grace; set --helm-timeout to the --timeout of your deploy]

Refused:   the operation may still be running, so the release is left alone (no flag overrides this: wait and run the command again)
           Run "helm-unstick explain web -n prod" for the full picture.
exit=3
```

Then the CI job dies (`kill -9`). Once the record is older than the helm timeout plus a minute, nothing can still be waiting, and `fix` recovers the release:

```console
$ helm-unstick scan -n prod --older-than 0s --helm-timeout 5s
NAMESPACE  RELEASE  STATUS           REV  AGE   LAST-DEPLOYED  VERDICT  ACTION
prod       web      pending-upgrade  2    1m7s  1              stale    fix: roll back to revision 1

Run "helm-unstick explain RELEASE -n NAMESPACE" for the reasons and the recovery plan.
exit=2

$ helm-unstick fix web -n prod --older-than 0s --helm-timeout 5s --yes
Release:   web (namespace prod)
Status:    pending-upgrade, revision 2, last written 1m7s ago
Chart:     slowapp-0.1.0
Deployed:  revision 1 is the newest deployed revision
Verdict:   stale
  - the pending revision was written 1m7s ago, more than the 0s threshold
  - the recent-change window is 0s, so object modification times were not considered
  - Service/web-lb, Deployment/web not ready, but the record is older than the Helm timeout (5s) plus grace, so any helm that waited on them has given up

Plan:      Roll back to revision 1, the newest deployed revision.
  1. Roll back release "web" to revision 1. Helm records the result as revision 3.
  2. Mark any of the pending revisions (2) that the rollback leaves behind as failed, so the history stops showing an operation in progress.
Commands:
  helm-unstick fix web -n prod --helm-timeout 5s --older-than 0s
    # recover with helm-unstick
  helm rollback web 1 -n prod
    # the rollback step alone, by hand

Rolling back web to revision 1 ...
Rollback path: direct
Revision 2 was left pending-upgrade by the interrupted operation and is now marked failed.
Done. Release web is deployed as revision 3 (content of revision 1). The next helm upgrade can proceed.
exit=0
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
| `stale` | The pending record is older than `--older-than` (default 10m), nothing shows activity, and no helm `--wait` can still be waiting. | acts |
| `possibly-running` | The record is younger than the threshold, or at least one sign of activity was found. | refuses, exit 3, no override |
| `unknown` | Some check could not be completed. | refuses, exit 3, unless `--force-unknown` |

### Signs of activity

They come from the objects in the manifest and the hooks of the pending revision.

1. **Recent changes.** An object was modified inside the `--older-than` window (newest `managedFields` timestamp, any manager).
2. **What `helm --wait` would still be waiting on.** The rules follow Helm 3's `ReadyChecker` (`pkg/kube/ready.go`), and use the stricter `kubectl rollout status` rules where those differ:
   - Pods must be `Ready`, PersistentVolumeClaims `Bound`, Services must have a cluster IP and, for a `LoadBalancer` without external IPs, an ingress address;
   - Deployments: the controller must have observed the latest generation before any condition counts. A Deployment past its progress deadline still counts: Helm 3 keeps waiting there (Helm 4 stops). StatefulSets honor `partition` and `currentRevision`/`updateRevision`. DaemonSets, ReplicaSets and CRDs (`Established`) are checked too;
   - hook Jobs and hook Pods have not finished, or a Job has neither completed nor failed;
   - custom resources that report `Ready=False` or `Reconciling=True` (what Helm 4 waits on).
3. Objects whose name Kubernetes picked (`generateName`) are found by name prefix and creation time.

Activity of kind 2 only proves that a helm may still be waiting, and Helm gives up after its `--timeout`. So it blocks the verdict only while the pending record is younger than `--helm-timeout` plus a minute of grace. **Set `--helm-timeout` to the `--timeout` your deploys pass to helm** (default 5m, Helm's own default). Helm applies `--timeout` to each hook and to the final wait separately, so a release with slow hooks can legitimately run longer: cover the total. Recent changes (kind 1) never expire this way.

### What makes a verdict `unknown`

- an object cannot be read (`Forbidden`, API errors);
- the cluster does not serve the kind of an object in the manifest (CRD not installed, API version removed), even after trying the other served versions;
- the record has no timestamp, or the storage records cannot be listed;
- a check fails halfway.

A release whose storage records cannot be decoded is never guessed at: `explain` and `fix` fail, and `scan` names the release and exits with code 1.

### Guards around the write

- The age is the later of `info.last_deployed` and the write time of the storage record (`createdAt` or `modifiedAt`). It is compared with the clock of the machine running `helm-unstick`, so a large skew against the cluster can shift the result. A timestamp in the future counts as "too young".
- After the confirmation prompt (and with `--yes`, right before acting) `fix` reads the history and inspects the cluster again. It stops if the record changed or the verdict is not the same as the one it showed.
- Marking a revision `failed` is conditional. The write carries the `resourceVersion` of the record that was inspected, so the API server rejects it if anything touched the record since, and a record that is no longer pending (a live helm just finished) is never rewritten.
- `--strategy auto` falls back to mark-failed only when Helm refused with "another operation is in progress". RBAC, transport and validation errors are returned without further writes.
- After a rollback, `fix` checks the revision it created itself. If another operation wrote a newer revision meanwhile, it reports the concurrent operation instead of success.
- Ctrl-C and SIGTERM stop the command before its next write.

`--older-than 0s` switches the age rule and the modification window off. Objects a helm `--wait` may still wait on keep blocking until `--helm-timeout` plus grace has passed.

## Recovery matrix

| Latest revision | Deployed revision in history | What `fix` does |
| --- | --- | --- |
| `pending-upgrade` | yes | Roll back to the newest deployed revision. |
| `pending-rollback` | yes | Roll back to the newest deployed revision. |
| `pending-install` | not applicable | Print the plan only. Continue with `--first-install=mark-failed` or `--first-install=uninstall`. |
| `pending-upgrade`, `pending-rollback` | none, but a superseded revision | Print the plan only. Helm stores the old revision as `superseded` before it stores the new `deployed` one, so an interruption between the two writes looks like this. Roll back to the newest superseded revision N only with an explicit `--to-revision N`. |
| `pending-upgrade`, `pending-rollback` | none at all | The same as an interrupted first install. |
| anything else | | Nothing to do. |

An interrupted first install has no earlier state to return to, so `helm-unstick` does not choose for you:

- `--first-install=mark-failed` sets the pending revision to `failed`. The resources the install created stay, and the next `helm upgrade --install` proceeds.
- `--first-install=uninstall` removes the release and its stored history, like `helm uninstall`. Objects with `helm.sh/resource-policy: keep` and volumes created from StatefulSet `volumeClaimTemplates` are not removed.

Without the flag `fix` prints the options with exact commands and exits with code 2. `--first-install=uninstall` deletes the whole history, and the plan warns about that when the release has earlier revisions.

### How the rollback runs

`fix` rolls back with the Helm SDK. Whether the SDK accepts a rollback while the latest revision is still `pending-*` was checked with e2e runs on kind:

| Helm CLI that left the release stuck | `pending-upgrade` | `pending-rollback` |
| --- | --- | --- |
| Helm 3.22.0 | direct rollback works | direct rollback works |
| Helm 4.3.0 | direct rollback works | direct rollback works |

So no `pending -> failed` step is needed first, and `--strategy mark-failed` (which does that step and then rolls back) works as well. `auto` keeps the fallback in case a future Helm refuses. Choose with `--strategy`:

| Strategy | Behavior |
| --- | --- |
| `auto` (default) | Try the rollback directly. If Helm refuses with "another operation is in progress" and the pending record is untouched, mark it `failed` and try again. Any other error is returned as it is. |
| `direct` | Try the rollback directly and report the error. |
| `mark-failed` | Mark the pending revision `failed`, then roll back. |

The output names the path that worked (`Rollback path: ...`). `make e2e` prints the path each run took.

After a successful rollback, any older revision that still says `pending-*` is set to `failed`, so `helm history` no longer shows an operation in progress. The rollback runs Helm's rollback hooks, as `helm rollback` does.

## What it does not do

- It deletes release records only for a confirmed `--first-install=uninstall`, which runs `helm uninstall` and removes the whole history. Everything else keeps them.
- It never touches a release whose latest revision is not `pending-*`. A `failed` release is a different problem.
- It does not repair broken workloads. A rollback returns the release to the last deployed revision, and that is all.
- It cannot prove an operation is dead. It can only fail to find any sign of life, and it says which checks it ran. A helm with a `--timeout` longer than `--helm-timeout` plus a minute can outlive the protection: that is why the flag exists.
- It does not model kinds that neither Helm 3 nor Helm 4 waits on, and it reads custom resources only through the `Ready` and `Reconciling` conditions.
- It does not need a `helm` binary. It embeds the Helm 3 SDK (`helm.sh/helm/v3`) and reads and writes the `sh.helm.release.v1` records that both Helm 3 and Helm 4 use. The e2e suite creates the stuck states with the Helm 4 CLI too and recovers them with this SDK, and the follow-up `helm upgrade` from Helm 4 succeeds. The v3 module is used because it already covers what is needed (history, storage update, rollback, uninstall) for both CLIs; the `helm.sh/helm/v4` SDK was not needed and not tried.

## Commands

```text
helm-unstick scan    [-n NAMESPACE | -A] [--older-than 10m] [--helm-timeout 5m] [-o table|json]
helm-unstick explain RELEASE [-n NAMESPACE] [--older-than 10m] [--helm-timeout 5m]
helm-unstick fix     RELEASE [-n NAMESPACE] [--dry-run] [--yes] [--older-than 10m] [--helm-timeout 5m]
                     [--force-unknown] [--first-install uninstall|mark-failed] [--to-revision N]
                     [--strategy auto|direct|mark-failed] [--wait] [--timeout 5m]
```

Common flags: `--kube-context`, `--kubeconfig` (`KUBECONFIG` works as in kubectl), `--driver secret|configmap` (default `$HELM_DRIVER`, then `secret`). The commands printed in plans repeat `--kube-context` and `--kubeconfig`, and add `HELM_DRIVER=...` for a non-default driver, so a pasted `helm rollback` acts on the cluster that was analyzed.

`--older-than` is how old a pending record must be, and how recent a modification must not be. `--helm-timeout` is the `--timeout` of the helm run that may still be alive. `--timeout` of `fix` is a different thing: the timeout of the rollback that `fix` runs itself.

`scan` lists every release whose latest revision is pending, with the verdict and the recommended action. `-o json` gives an array for scripts. `explain` prints the reasons, the recovery plan with exact commands, and the revision history. `fix --dry-run` prints the plan and changes nothing. Without `--yes`, `fix` asks for confirmation and refuses to run when stdin is not a terminal.

| Exit code | Meaning |
| --- | --- |
| 0 | Done, or nothing is stuck. |
| 1 | Error, or a `scan` that could not read every release (then it never says "nothing is stuck"). |
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

As a Helm plugin (the hook downloads the release binary and checks its checksum). Helm 3:

```console
$ helm plugin install https://github.com/DanilaZanin/helm-unstick --version v0.1.0
$ helm unstick scan -A
```

Helm 4 verifies plugin signatures by default and a Git source has none, so it refuses the command above. Say so explicitly and pin the version:

```console
$ helm plugin install https://github.com/DanilaZanin/helm-unstick --version v0.1.0 --verify=false
```

The release binary is checked against `checksums.txt` by the install hook either way. To install from a local checkout with a binary you built (CI does this on Helm 3 and Helm 4): `HELM_UNSTICK_BINARY=bin/helm-unstick helm plugin install .`

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
$ make e2e         # kind cluster, eight scenarios; HELM=/path/to/helm4 picks the Helm CLI
```

The e2e script (`test/e2e.sh`) uses the Helm CLI under test to start `helm upgrade --wait`, `helm install --wait` and `helm rollback --wait` against a chart with a slow readiness probe, kills the client with `kill -9`, and checks what `helm-unstick` does:

1. An interrupted upgrade is `stale`, `fix` restores the previous revision, and the next `helm upgrade` passes. It runs once per `--strategy` and records which path each one took.
2. A live slow upgrade is `possibly-running`, `fix` exits with 3, and `--force-unknown` does not change that.
3. An interrupted first install: `fix` alone prints the plan and changes nothing. Then `--first-install=mark-failed` lets `helm upgrade --install` pass, and `--first-install=uninstall` lets `helm install` pass.
4. An interrupted rollback returns to the last deployed revision.
5. Helm 3 only: `helm upgrade --wait` keeps waiting after the Deployment exceeded its progress deadline. While that helm is alive `fix` refuses with 3, also right after it is killed. Once the record is older than the helm timeout plus grace, `fix` recovers and the history has exactly one `deployed` revision.
6. Helm 3 only: a LoadBalancer Service without an ingress address keeps helm waiting; the verdict names the Service and `fix` refuses. Helm 4.3.0 does not wait for the address.
7. A release record that does not decode makes `scan` exit with 1 and name the record.
8. `helm plugin install` from the local directory works on the Helm generation under test, and `helm unstick scan` runs.

`E2E_ONLY="live_wait plugin" test/e2e.sh` runs a subset. CI runs the suite against the newest Helm 3 and Helm 4 (verified locally with 3.22.0 and 4.3.0). The release workflow calls the whole CI workflow on the tagged commit and publishes only after it passes.

Layout: `internal/model` (history and stuck detection), `internal/verdict` and `internal/liveness` (the liveness rules), `internal/plan` (recovery plans), `internal/app` (the commands, against a `Backend` interface), `internal/cluster` (the Helm SDK and client-go implementation).

## License

MIT. See [LICENSE](LICENSE).
