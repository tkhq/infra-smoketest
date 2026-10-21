# Infrastructure smoke-test controller

A Kubernetes controller that runs declarative infrastructure checks and stores
their results in `SmokeTestRun` resources. Definitions validate
Cluster Autoscaler scale-up, EBS CSI volume persistence, the AWS IRSA
pod-identity webhook, cert-manager issuance, and kube-state-metrics observation.
See [examples](examples) for the current tests.

The controller and probes are subcommands of **one binary and one image**.
Unless `--probe-image` is supplied, the controller reads its own Pod and uses
the `controller` container's `spec.image` for all probe Pods. Each Run snapshots
that reference before creating resources. Updating the Deployment image is
enough to update the default probes for new Runs; no second image setting is
required. Prefer a digest in the Deployment so the same reference always
identifies the same build.

## Build and test

Install Nix with flakes enabled, then use the locked development environment:

```sh
nix develop path:.
make generate
make check
make build
```

`make check` runs vet, race-enabled unit tests, API-server integration tests,
manifest builds, Terraform validation and mocked tests, formatting checks, and
GitHub Actions linting. The flake provides Go, controller-gen, kube-apiserver,
etcd, kubectl, Kustomize, Terraform, and the other tools. Envtest does not download
executables at test time. Integration tests start local API-server/etcd processes
and simulate infrastructure status.
They do not connect to your cluster or AWS account.

Nix also builds the package and container without a Docker daemon:

```sh
nix build path:.#default     # result/bin/infra-smoketest
nix build path:.#image       # result is a Docker image archive
```

The image contains both `controller` and `probe` commands, a CA certificate
bundle, and runs as UID/GID 65532. The default command starts the controller;
the generated probe Pods select their subcommand through native Kubernetes
`command` and `args`. Linux x86_64 and aarch64 flake outputs are defined; the
initial GitHub Actions workflow builds and publishes x86_64 images.

When changing dependencies, run `nix develop path:. --command go mod tidy`,
update the flake's `vendorHash` using the hash reported by a Nix build, and rerun
the checks. Keep the Go module lock and `flake.lock` in version control.

## GitHub Actions

[ci.yaml](.github/workflows/ci.yaml) runs for PRs and pushes to `main` that change
`api/`, `cmd/`, `internal/`, Go dependencies, the Nix flake or lockfile,
`Makefile`, `scripts/`, or `.github/workflows/`. Changes confined to `config/`,
`examples/`, or the root README do not trigger the workflow. Release pushes with
`v*` tags and manual workflow runs still run regardless of changed paths.

The workflow uses the same flake, verifies generated CRDs/DeepCopy code, and
builds the combined controller/probe image. After successful push builds, it publishes to
`ghcr.io/<repository-owner>/<repository-name>` using `GITHUB_TOKEN`:

- `sha-<full-commit>` for every published build.
- `latest` on `main`.
- The version tag on `v*` tag pushes.

PRs and manual workflow runs do not publish. Actions are pinned to commits.
Publishing requires the repository's Actions token to have package write
access. For private images, configure `imagePullSecrets` on the controller
Deployment and the probe ServiceAccounts. Nothing has been pushed or
published by preparing this repository.

## Install and run

The manifests in [config/default](config/default) install the namespace,
controller Deployment, controller/probe/identity/cert-manager ServiceAccounts,
RBAC, and all five `SmokeTest` definitions from [examples](examples).
Install the CRDs from
[config/crd](config/crd) first. The controller is restricted to the
`infra-smoketest` namespace and has read-only cluster permissions. It does not
create IAM roles or ASGs. Kustomize installs the identity ServiceAccount;
the controller only reads it.

Before a live test, arrange these prerequisites through their owning repos:

- Autoscaling: a discoverable scale-from-zero ASG whose nodes have
  `turnkey.engineering/designated-for=smoke-tests` and the matching `NoSchedule`
  taint. Mirror the selector/taint in ASG node-template tags. Other workloads
  must not resize or use this pool during the test.
- Storage: the existing `ebs-gp3` class, EBS CSI nodes on ordinary AWS workers,
  and `Delete` reclaim policy. The controller checks gp3/encryption configuration
  and Kubernetes CSI state; it does not query actual EBS volume properties.
- Identity: [config/aws-pod-identity-webhook](config/aws-pod-identity-webhook)
  creates `infra-smoketest/infra-smoketest-aws` and is included in `config/default`.
  Its `eks.amazonaws.com/role-arn` annotation in
  [serviceaccount.yaml](config/aws-pod-identity-webhook/serviceaccount.yaml) is
  the single source of the expected AWS identity: the webhook injects it into
  identity Pods, and the controller reads the same annotation when it accepts a
  Run, deriving the account ID from it. Override the annotation per environment
  with a Kustomize patch. The default is
  `arn:aws:iam::361645878370:role/infra-smoketest-aws`, matching the existing
  `tvc-dev` GitOps configuration.
  The role's OIDC trust must allow subject
  `system:serviceaccount:infra-smoketest:infra-smoketest-aws` and audience
  `sts.amazonaws.com`. The role needs no attached AWS service policy for
  `GetCallerIdentity`. The [Terraform module](config/terraform/aws) creates this
  role using the cluster's existing IAM OIDC provider; its runnable example reads
  the same annotation. Region defaults to `us-east-1` and can be set with
  `--region`.
- Certificates: cert-manager's `cert-manager.io/v1` Issuer and CertificateRequest
  APIs, webhook, issuer controller, request approver, and self-signed signer must
  operate in `infra-smoketest`. The dedicated probe ServiceAccount can create
  and read Issuers/CertificateRequests and create Secrets in that namespace.
  It also has read-only `get` access to ClusterIssuers for optional existing
  issuer selection.
  Custom approval policies must permit its requests for a namespaced SelfSigned
  Issuer, or for the configured existing issuer. With the default configuration,
  no production issuer, external CA, DNS credentials, or ACME account is required.
- Metrics: the probe must reach the kube-state-metrics Service, which must watch
  Pods in `infra-smoketest` and expose `kube_pod_info` and `kube_pod_status_phase`.
  The example targets `http://kube-state-metrics.kube-state-metrics.svc:8080/metrics`,
  matching the GitOps namespace. Override the literal `KUBE_STATE_METRICS_URL`
  container environment variable in the definition for another Service. This
  probe supports HTTP or HTTPS using the image's trusted CAs; it sends no API
  token or other credentials and does not follow redirects.

When a Run is accepted, the controller reads the identity ServiceAccount's role
annotation and saves the role as `status.expectedRoleARN`, its account as
`status.awsAccountID`, and the configured `status.region`. Every probe receives
`SMOKETEST_AWS_ACCOUNT_ID`, `SMOKETEST_EXPECTED_ROLE_ARN`, and `SMOKETEST_REGION`
from that snapshot; definitions do not need to repeat them. These are controller
supplied environment variables, not substitution expressions in SmokeTest YAML.
New probe implementations can read the account directly. The identity probe
already checks the caller account through the expected role ARN.

The controller can optionally pin the expectation instead of trusting the
annotation: `--aws-account-id=ACCOUNT` expects
`arn:aws:iam::ACCOUNT:role/infra-smoketest-aws`, `--identity-role-name=NAME`
selects a different role in that account, and `--expected-role-arn` pins a full
ARN, including IAM paths and other partitions; when also supplying an account or
role name, they must match the ARN. A pinned Run only creates its identity Pod
if the ServiceAccount annotation matches the pin, which guards against the
annotation being pointed at another account. Without an annotation or pin,
autoscaler, storage, certificate, and metrics tests can still run, while the
identity test fails its configuration precondition.

Changing the annotation takes effect for the next accepted Run without a
controller restart. Already accepted Runs retain their snapshot, and the
annotation must still match a Run's saved role before it can create an identity
Pod.

Make a deployment overlay to select your published image digest, for example:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../config/default
images:
  - name: ghcr.io/tkhq/infra-smoketest
    newName: ghcr.io/YOUR-OWNER/YOUR-REPOSITORY
    digest: sha256:YOUR-DIGEST
```

The shared account flags require a controller image built from this revision.
Build/publish it and update the pinned digest before applying the Deployment.

Install the CRDs, then apply your overlay. The overlay includes the definitions
through `config/default`; no separate examples installation is needed. Installing
the definitions does not start a test; create a fresh Run for every execution:

```sh
kubectl apply --server-side -k config/crd
kubectl wait --for=condition=Established --timeout=60s \
  crd/smoketests.smoketest.turnkey.engineering \
  crd/smoketestruns.smoketest.turnkey.engineering
kubectl apply --server-side -k path/to/your-overlay
kubectl get smoketests -n infra-smoketest
kubectl create -f - <<'YAML'
apiVersion: smoketest.turnkey.engineering/v1alpha1
kind: SmokeTestRun
metadata:
  generateName: autoscaler-
  namespace: infra-smoketest
spec:
  testRef:
    name: cluster-autoscaler
  context:
    trigger: manual
YAML
kubectl get smoketestruns -n infra-smoketest
kubectl get smoketestrun RUN-NAME -n infra-smoketest -o yaml
```

Use server-side apply for installation: embedding native Kubernetes template
schemas makes these CRDs too large for some client-side apply annotations.

`spec.testRef` selects a `SmokeTest` definition in the Run's namespace. List
available definitions with `kubectl get smoketests -n infra-smoketest`, then set
`testRef.name` to one of the returned names. The default deployment installs
`cluster-autoscaler`, `ebs-csi`, `aws-pod-identity-webhook`, `cert-manager`, and
`kube-state-metrics`. Optional `testRef.uid`
and `testRef.generation` require a matching definition UID and generation;
otherwise, the Run snapshots the current definition when accepted.

Use `kubectl explain smoketestrun.spec.testRef` or
`kubectl explain smoketest.spec.stages` for field descriptions.

For the new services, create Runs the same way, with `testRef.name: cert-manager`
or `testRef.name: kube-state-metrics`. Build and deploy the updated combined image
and reapply the CRD and default manifests first: these probes require the new
commands, result fields, and namespace RBAC.

The [cert-manager definition](examples/cert-manager.yaml) generates a fresh ECDSA
key and CSR for a Run-specific `.invalid` DNS name. By default it creates a temporary
[SelfSigned Issuer](https://cert-manager.io/docs/configuration/selfsigned/), and
submits a CertificateRequest. It waits for the Issuer and request to become Ready,
then verifies the returned certificate's key, exact DNS name, self-signature,
validity period, and server-auth usage. Its SHA-256 fingerprint is saved as
`status.resources[].result.certificateSHA256`. This exercises admission, approval,
and signing; it does not test the Certificate controller's renewal behavior,
ACME/DNS challenges, or CA injection.

To use an existing issuer, add literal environment values to the `probe` container
in the definition's `issue` stage (under
`spec.stages[].createPod.template.spec.containers[]`):

```yaml
env:
  - name: CERT_MANAGER_ISSUER_NAME
    value: cluster-ca
  - name: CERT_MANAGER_ISSUER_KIND
    value: ClusterIssuer
```

`CERT_MANAGER_ISSUER_KIND` defaults to `Issuer`, which must exist in
`infra-smoketest`. `ClusterIssuer` selects a cluster-scoped issuer. Both use
the `cert-manager.io` API group. Omit both environment variables to use a new
temporary SelfSigned Issuer. A missing, inaccessible, or unusable explicit issuer
fails the test; it never silently falls back. Accepted Runs retain their selected
issuer configuration in the definition snapshot.

Existing issuers are only read and referenced, never created, modified, adopted,
or deleted by the test. With an existing SelfSigned issuer, the probe still creates
its temporary private-key Secret. For a CA-backed issuer it keeps the key in memory
and verifies the issued chain using the CertificateRequest's `status.ca` trust
bundle and any intermediates in `status.certificate`. If the issuer supplies no
CA bundle, verification uses the image's system trust roots. The probe does not
read the issuer's signing-key Secret or automatically trust the returned leaf.

The selected issuer and its approval policy must permit ECDSA P-256 server-auth
requests for `smoke-<hash>.invalid` with a requested duration of one hour. This
is suitable for internal CAs such as `cluster-ca`; the generated `.invalid` name
cannot be used for public ACME issuance. The probe and stage deadlines remain
unchanged when an existing issuer is selected.

The [kube-state-metrics definition](examples/kube-state-metrics.yaml) scrapes the
Service until both `kube_pod_info=1` and `kube_pod_status_phase{phase="Running"}=1`
identify the probe's exact namespace, Pod name, and UID. These
[Pod metrics](https://github.com/kubernetes/kube-state-metrics/blob/main/docs/metrics/workload/pod-metrics.md)
prove that kube-state-metrics observed a newly created object and serves its state.
A health response, unrelated samples, or metrics for a replaced Pod cannot pass.
The observed UID is saved as `status.resources[].result.metricsPodUID`. This checks
the Service's exposition endpoint, not Prometheus ingestion or alerting.

Both finite probes retry observations for up to three minutes after starting;
their result stages allow five minutes to include scheduling and image pulls.
Failures retain the last observation in the structured probe error.

`kubectl get smoketestruns` includes `FAILEDSTAGE`, taken from the existing
`status.stages` entries whose phase is `Failed`. For example, a timeout while
waiting for autoscaler scale-up shows `TimedOut` and `scale-up`. The stage remains
visible through cleanup and in the final result. Runs with no failed stage,
including execution-slot queue timeouts and missing-definition failures, leave
the column empty. Use `kubectl get smoketestruns -o wide` for the full `MESSAGE`,
including whether the queue, a stage, or the overall execution deadline expired.
For observation failures or cancellation, `FAILEDSTAGE` identifies the stage that
was interrupted; the message explains the underlying failure.

These columns also work with existing Runs after applying the updated CRD with
`kubectl apply --server-side -k config/crd`; no controller image update is needed.

`kubectl wait --for=condition=Complete ...` only waits for a terminal result:
also inspect `Succeeded` and `CleanupComplete`; completion alone does not mean
the test passed. The API rejects changes to an existing Run's spec.

The native Pod templates in [examples](examples) omit `containers[].image` to
inherit the controller image. An explicit template image must match the resolved
probe image; set `--probe-image` on the controller to deliberately change the
approved probe build. For local development outside a Pod, supply this flag
because no deployed controller image can be discovered:

```sh
nix develop path:. --command go run ./cmd/infra-smoketest controller \
  --probe-image=ghcr.io/YOUR-OWNER/YOUR-REPOSITORY@sha256:YOUR-DIGEST
```

## Controller metrics

The controller serves Prometheus metrics over HTTP at `/metrics` on port 8080.
The default manifests include the `infra-smoketest-metrics` ClusterIP Service.
Use `--metrics-bind-address=ADDRESS` to change the listener or
`--metrics-bind-address=0` to disable it; update the Service and container port
if changing the port.

```sh
kubectl port-forward -n infra-smoketest service/infra-smoketest-metrics 8080:8080
# In another terminal:
curl -fsS http://localhost:8080/metrics
```

For Prometheus Operator installations, an optional ServiceMonitor is provided:

```sh
kubectl apply -k config/monitoring
```

The Operator's ServiceMonitor CRD must already be installed. Configure your
Prometheus instance's `serviceMonitorSelector` and `serviceMonitorNamespaceSelector`
to include this monitor in `infra-smoketest`; add any required monitor labels
through your deployment overlay. Installing the Service alone does not configure
Prometheus scraping.

All custom metrics are gauges with a `smoketest` label from the Run's
`spec.testRef.name`. The `namespace` label is supplied by ServiceMonitor scraping
and is omitted from the exporter to avoid a label conflict.

| Metric | Additional labels | Meaning |
| --- | --- | --- |
| `infra_smoketest_runs` | `phase` | Number of retained Run objects in each current phase. |
| `infra_smoketest_cleanup_pending_runs` | None | Runs whose cleanup has started but `CleanupComplete` is not true, including failed Runs still holding the execution slot. |
| `infra_smoketest_oldest_pending_run_timestamp_seconds` | None | Creation timestamp of the oldest pending Run; absent when no Runs are pending. |
| `infra_smoketest_last_run_completed_timestamp_seconds` | `phase` | Latest completion timestamp per terminal phase, after cleanup has completed. |
| `infra_smoketest_last_run_duration_seconds` | `phase` | Duration from `startedAt` to `completedAt` for that latest completed Run, including cleanup and excluding queue time; absent if the Run never started. |

Run counts emit zero for each phase of every installed SmokeTest, including
definitions that have never run. Phases are `Pending`, `Running`, `CleaningUp`,
`Succeeded`, `Failed`, and `Cancelled`, plus `Unknown` for unrecognized status
values. Runs with no phase yet count as `Pending`. Completion and duration
metrics use only `Succeeded`, `Failed`, and `Cancelled`; they are absent until
a matching completed Run exists. A failed Run still cleaning up contributes to
the phase count and pending-cleanup count, but not the completion metrics.

Metrics are rebuilt from the Kubernetes watch cache on each scrape, so they
reflect retained objects across controller restarts and remove deleted Runs.
Completed Runs are normally retained for seven days. These are counts of current
objects, not cumulative counters: do not use `rate()` or `increase()` on them.
Run names, UIDs, and error messages are not metric labels. These choices follow
[Prometheus guidance on gauges, labels, and timestamps](https://prometheus.io/docs/practices/instrumentation/).
A cache read failure fails the scrape instead of reporting zero counts.

For example, inspect retained failures and time since the latest successful Run:

```promql
infra_smoketest_runs{phase="Failed"}
time() - infra_smoketest_last_run_completed_timestamp_seconds{phase="Succeeded"}
```

Every controller replica reports the same Run inventory, including followers.
If running multiple replicas, use `max` across replica labels instead of summing
their counts, keeping your cluster, namespace, SmokeTest, and phase labels.
The endpoint also exposes controller-runtime's reconciliation/error counters,
reconciliation duration, work-queue metrics, and Go/process metrics.

## Runtime behavior and limits

The controller logs Run acceptance, stage progress and deadlines, resource
creation/deletion, observed scheduling/storage evidence, and cleanup outcomes at
the default info level. Entries identify the Run, its UID, the test definition,
and the stage or resource where applicable. Unchanged waits are not logged on
every poll. Add `--zap-log-level=debug` to the controller arguments for reconcile
and waiting messages on each poll; `--zap-encoder=console` enables console output.

Follow logs with `kubectl logs -n infra-smoketest deployment/infra-smoketest -f`.
Run status remains the durable record of progress and results:
`kubectl get smoketestrun RUN-NAME -n infra-smoketest -o yaml`.

Run definitions, image references, stage deadlines, resource UIDs, transition
evidence, and probe results are persisted in Run status. Pod/PVC names are
deterministic; creation is recovered after a controller restart without adopting
unrelated resources. Probe image discovery uses the controller Pod's configured
image reference, not a runtime-specific `imageID`.

One execution Lease serializes all tests. A queued Run times out after ten
minutes; active execution allows 25 minutes plus five minutes for cleanup.
Submit longer batches sequentially. Cleanup deletes Run-owned Pods/PVCs
and waits for recorded CSI dependents to disappear. For cert-manager, it also
removes the temporary CertificateRequest, generated Issuer (if any), and
private-key Secret (if any) after
the probe Pod has stopped, verifying ownership against its recorded UID. This
runs on success, failure, timeout, and cancellation; the resources also carry
Pod owner references for garbage collection. Existing issuers are excluded from
cleanup. Cleanup must finish before the Run
passes or releases its execution slot. The key is never included in results or
diagnostics. Only the cert-manager probe receives a short-lived projected API
token; ordinary probes still have no Kubernetes API credentials.
The controller never deletes Nodes,
PVs, VolumeAttachments, or AWS resources directly. If cleanup stalls, the Run
fails, retains its finalizer and execution slot, and keeps attempting cleanup.
Inspect the remaining Pod/PVC/PV/attachment and repair its underlying problem;
do not remove finalizers to force a passing result. Drain Runs before removing
the controller or CRDs. A Lease referring to a forcibly removed Run requires
manual recovery after verifying that its resources have been cleaned up.

Cleaned Run records and their diagnostic ConfigMaps are retained for seven
days. The initial diagnostic artifact contains bounded status evidence; richer
Event/log bundles and infrastructure version inventory remain follow-on work.
Scale-down is not a required assertion. No live AWS/cluster validation is
claimed by the local tests: the smoke ASG and IAM prerequisites are still
required for acceptance in dev and preprod. GitOps deployment and promotion
integration remain separate work.
