# SmokeTest examples

These definitions run infrastructure checks in the `infra-smoketest` namespace.
A `SmokeTest` describes a test; creating a `SmokeTestRun` executes it. The
[default deployment](../config/default/kustomization.yaml) includes all five
definitions through [kustomization.yaml](kustomization.yaml).

| Definition / `testRef.name` | What it verifies |
| --- | --- |
| [cluster-autoscaler](cluster-autoscaler.yaml) | An unschedulable Pod triggers scale-up and becomes ready on a newly created AWS Node. |
| [ebs-csi](ebs-csi.yaml) | A fresh EBS volume binds, attaches, and preserves data across replacement of its consuming Pod. |
| [aws-pod-identity-webhook](aws-pod-identity-webhook.yaml) | The webhook injects web-identity credentials and the Pod authenticates as the expected AWS role. |
| [cert-manager](cert-manager.yaml) | An issuer produces a valid certificate for a fresh key and the requested DNS name. |
| [kube-state-metrics](kube-state-metrics.yaml) | The metrics endpoint exposes the new probe Pod's identity and Running state. |

## Run a test and check the result

First install the CRDs, controller, ServiceAccounts, and RBAC using the
[installation instructions](../README.md#install-and-run). The commands below
assume that installation is complete and are run from the repository root.
To apply just the definitions to an existing installation:

```sh
kubectl apply --server-side -k examples
```

Create a fresh Run for each execution. Change `testRef.name` to select another
definition from the table above:

```sh
kubectl create -f - <<'YAML'
apiVersion: smoketest.turnkey.engineering/v1alpha1
kind: SmokeTestRun
metadata:
  generateName: cert-manager-
  namespace: infra-smoketest
spec:
  testRef:
    name: cert-manager
  context:
    trigger: manual
YAML
```

Use the generated Run name returned by `kubectl create`:

```sh
run=YOUR_RUN_NAME
ns=infra-smoketest

kubectl wait -n "$ns" "smoketestrun/$run" \
  --for=condition=Complete --timeout=40m &&
kubectl wait -n "$ns" "smoketestrun/$run" \
  --for=condition=Succeeded --timeout=1s &&
kubectl wait -n "$ns" "smoketestrun/$run" \
  --for=condition=CleanupComplete --timeout=1s
```

All three commands must succeed. `Complete=True` also includes failed or
cancelled Runs. Inspect the failed stage, message, and recorded evidence with:

```sh
kubectl get smoketestrun "$run" -n "$ns" -o wide
kubectl get smoketestrun "$run" -n "$ns" -o yaml
```

## Shared behavior and configuration

- Configure the SmokeTest definitions before creating a Run, either by editing
  their YAML or patching them in your deployment's Kustomize overlay. Each Run
  snapshots its definition; changes affect newly accepted Runs. `spec.context`
  on a Run is descriptive metadata and does not supply test parameters.
- Templates omit the container image to inherit the controller's deployed
  image. The controller's `--probe-image` flag overrides that choice. Custom
  template images must match the approved probe image.
- The controller schedules the autoscaler probe on the dedicated smoke pool.
  All other probes run on AWS workers with
  `turnkey.engineering/cloud-platform=aws` and exclude that pool.
- Stage `timeout` values are configurable Go durations, greater than zero and
  at most `25m`. A create stage completes when its resource is created; the
  following assertion stage waits for functionality. Finite probe processes
  have their own three-minute deadline after starting, which a longer stage
  timeout does not extend.
- One execution Lease serializes Runs. Queueing allows ten minutes, execution
  allows 25 minutes, and cleanup has a five-minute deadline. Submit large
  batches sequentially to avoid queue timeouts.
- Cleanup runs after success, failure, timeout, or cancellation. A Run succeeds
  only after its assertions and cleanup both pass. A cleanup failure retains
  the finalizer and execution slot while the controller keeps retrying.
- Temporary resources disappear during cleanup. Run status and a diagnostic
  ConfigMap containing `result.json` retain the evidence for seven days.

## Cluster Autoscaler

Definition: [cluster-autoscaler.yaml](cluster-autoscaler.yaml).

Stages: `empty-pool` (5m), `demand` (1m), `scale-up` (15m).

The test starts by waiting for the smoke pool to contain no Kubernetes Nodes
and recording the existing Node UIDs. It creates a Pod that selects the smoke
pool and requires all of the following:

- Evidence that the Pod was initially unschedulable.
- A `TriggeredScaleUp` Event from Cluster Autoscaler for that exact Pod UID.
- Scheduling onto a Node absent from the baseline and created after the Pod.
  The Node must have the smoke-pool label, an AWS provider ID and cloud-platform
  label, and no cloud-provider initialization taint.
- `Ready=True` on both the new Node and the probe Pod. The Pod's readiness
  probe calls its local `/readyz` endpoint.

Provision a scale-from-zero ASG discoverable by Cluster Autoscaler. Its Nodes
must have the label `turnkey.engineering/designated-for=smoke-tests` and the
matching `NoSchedule` taint. Mirror that label and taint in the ASG's
node-template tags. Keep other workloads from using or resizing the pool during
the test. The controller does not create the ASG.

There are no test-specific environment options. The smoke-pool label and value
are fixed by the controller's validation. The template includes the matching
selector and toleration.

Cleanup deletes the demand Pod. The test does not require scale-down, delete
Nodes, or directly inspect ASG state in AWS.

## EBS CSI

Definition: [ebs-csi.yaml](ebs-csi.yaml).

Stages: `claim` (1m), `writer` (1m), `persisted` (5m), `remove-writer` (1m),
`reader` (1m), `verified` (5m).

The test creates a fresh 1Gi `ReadWriteOnce` filesystem PVC. A writer Pod mounts
it at `/data`, writes and syncs a Run-specific sentinel, and reports its SHA-256
checksum. The assertions require:

- A Bound PVC with a newly provisioned EBS CSI PV correlated to this claim's UID,
  an EBS volume handle, and a `Delete` reclaim policy.
- An attached EBS `VolumeAttachment` correlated with the consumer Node, plus
  EBS CSI registration on that Node.
- A successful writer result containing the expected sentinel checksum.
- After deleting the writer, a new reader Pod on the same Node reads the same
  PVC and returns the expected checksum.

The existing `ebs-gp3` StorageClass must use `ebs.csi.aws.com`,
`WaitForFirstConsumer`, `type: gp3`, `encrypted: "true"`, and a `Delete` reclaim
policy. EBS CSI must operate on the AWS workers hosting the probes.

There are no test-specific environment options. The current validator requires
`ebs-gp3`, a 1Gi RWO filesystem claim, and the `/data` mount; these values cannot
be changed merely by substituting another class or volume size in the example.

Cleanup deletes the Pods and PVC, then waits for the recorded PV and attachment
to disappear. This verifies persistence across Pod replacement on the same
Node. It does not test recovery on a different Node or query AWS to verify the
actual EBS volume's type, encryption, or deletion.

## AWS pod-identity webhook

Definition: [aws-pod-identity-webhook.yaml](aws-pod-identity-webhook.yaml).

Stages: `identity` (1m), `authenticated` (3m).

The test creates a Pod using `infra-smoketest-aws` and the
`pod-identity-webhook: required` label. It verifies:

- Admission injected the expected IAM role ARN, token-file path, AWS region,
  and regional STS endpoint setting.
- The Pod has a read-only projected service-account token with audience
  `sts.amazonaws.com`, mounted at the injected token-file path.
- The probe authenticates using the web-identity credential provider and
  successfully calls STS `GetCallerIdentity`.
- The returned account and assumed-role ARN match the configured account and
  IAM role. Static credentials, container credential sources, and fallback to
  the worker Node's role cannot satisfy the test.

Set the expected role on
[the identity ServiceAccount](../config/aws-pod-identity-webhook/serviceaccount.yaml):

```yaml
metadata:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/infra-smoketest-aws
```

The controller reads this annotation when it accepts a Run and derives the
account ID from it, so there is nothing else to configure. The repository
default is `arn:aws:iam::361645878370:role/infra-smoketest-aws`. Region
defaults to `us-east-1`; configure the controller's `--region` flag to change
the expected region and keep the webhook's region configuration consistent.

To pin the expectation independently of the annotation, use `--aws-account-id`
and optionally `--identity-role-name`, or `--expected-role-arn` for a full ARN.
The ServiceAccount annotation must then match. These are controller settings,
not environment variables to add to the example probe container.

The IAM role must trust the cluster's OIDC provider for subject
`system:serviceaccount:infra-smoketest:infra-smoketest-aws` and audience
`sts.amazonaws.com`. The [Terraform module](../config/terraform/aws/README.md)
documents role provisioning. The webhook and connectivity to regional STS must
also be available.

Cleanup deletes the probe Pod and retains the shared ServiceAccount and IAM
role. This checks authentication, not permissions to access other AWS services.

## cert-manager

Definition: [cert-manager.yaml](cert-manager.yaml).

Stages: `issue` (1m), `issued` (5m).

The probe generates a fresh ECDSA P-256 key and CSR for
`smoke-<hash>.invalid`, requesting a one-hour certificate with digital-signature
and server-auth usages. By default, it creates a temporary SelfSigned `Issuer`,
a private-key Secret, and a `CertificateRequest`. It verifies:

- The Issuer and CertificateRequest become `Ready=True` without being replaced.
- The returned certificate matches the private key and exact requested DNS name.
- The certificate is currently valid and usable for server authentication.
- For a SelfSigned issuer, the self-signature is valid. For a CA-backed issuer,
  the chain verifies using the request's `status.ca` bundle and intermediates in
  `status.certificate`; if no CA bundle is supplied, it uses system trust roots.

The result stores the verified certificate's fingerprint in
`status.resources[].result.certificateSHA256`. The certificate itself is returned
in `CertificateRequest.status.certificate`. This test creates no `Certificate`
resource and does not test renewal or cainjector.

The cert-manager APIs, admission webhook, issuer controller, request approval,
and signer must be functional. The default installation supplies the dedicated
`infra-smoketest-cert-manager` ServiceAccount and RBAC. It is the only probe
account that receives a projected Kubernetes API token.

To use an existing issuer, add `env` to the `probe` container in the `issue`
stage, at `spec.stages[].createPod.template.spec.containers[]`:

```yaml
env:
  - name: CERT_MANAGER_ISSUER_NAME
    value: cluster-ca
  - name: CERT_MANAGER_ISSUER_KIND
    value: ClusterIssuer
```

| Setting | Default | Supported values |
| --- | --- | --- |
| `CERT_MANAGER_ISSUER_NAME` | Omitted: create a temporary SelfSigned Issuer | An existing issuer's resource name |
| `CERT_MANAGER_ISSUER_KIND` | `Issuer` | `Issuer` or `ClusterIssuer`; requires a name |

Both kinds use the `cert-manager.io` API group. A namespaced `Issuer` must exist
in `infra-smoketest`; a `ClusterIssuer` is cluster-scoped. Use literal environment
values. An explicit issuer that is missing, inaccessible, unready, or unable to
sign fails the test without falling back to a generated issuer.

The requested `.invalid` DNS name is fixed by the probe and does not need to
resolve for this test. It is unrelated to the cluster's DNS suffix, often
`cluster.local`. The issuer and its approval policy must allow this test name and
the requested key type and usages. This configuration suits internal CAs such
as `cluster-ca`; public ACME issuance cannot use the generated `.invalid` name.

Existing issuers are only read and referenced. For an existing SelfSigned
issuer, the probe creates its own temporary signing-key Secret. With a CA-backed
issuer, the new key stays in memory; the probe never reads the issuer's key.
Cleanup removes the probe Pod, request, generated issuer if present, and
temporary key Secret if present. It never modifies or deletes a selected
existing issuer. A Run succeeds only once those temporary resources are gone.

## kube-state-metrics

Definition: [kube-state-metrics.yaml](kube-state-metrics.yaml).

Stages: `scrape` (1m), `observed` (5m).

The probe repeatedly scrapes the metrics Service until a successful response
contains both of these samples for its exact namespace, Pod name, and UID:

- `kube_pod_info = 1`
- `kube_pod_status_phase{phase="Running"} = 1`

This verifies that kube-state-metrics observed a newly created Pod and exposes
its state. An HTTP success alone, unrelated metrics, or a sample from an older
Pod with the same name cannot pass. The observed UID is retained in
`status.resources[].result.metricsPodUID`.

kube-state-metrics must watch Pods in `infra-smoketest`, enable those metric
families, and expose an endpoint reachable from the probe. The default URL is:

```text
http://kube-state-metrics.kube-state-metrics.svc:8080/metrics
```

To use another Service, change the literal environment value on the `probe`
container in the `scrape` stage:

```yaml
env:
  - name: KUBE_STATE_METRICS_URL
    value: http://kube-state-metrics.monitoring.svc:8080/metrics
```

Omitting the variable uses the default URL. HTTP and HTTPS are supported; HTTPS
uses the image's trusted CAs. The probe sends no API token or other credentials
and does not follow redirects. Scrapes have a ten-second request timeout and a
32 MiB response limit, within the finite probe's three-minute deadline.

Cleanup deletes the probe Pod. This test covers the metrics endpoint and its
view of the probe Pod, not Prometheus ingestion, dashboards, or alert delivery.
