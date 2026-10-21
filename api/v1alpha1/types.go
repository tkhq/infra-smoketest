// Package v1alpha1 defines the smoke-test API.
// +kubebuilder:object:generate=true
// +groupName=smoketest.turnkey.engineering
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var GroupVersion = schema.GroupVersion{Group: "smoketest.turnkey.engineering", Version: "v1alpha1"}
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
var AddToScheme = SchemeBuilder.AddToScheme

func init() {
	SchemeBuilder.Register(&SmokeTest{}, &SmokeTestList{}, &SmokeTestRun{}, &SmokeTestRunList{})
}

// SmokeTest defines a reusable infrastructure smoke test as a set of dependent
// stages. Creating a SmokeTest does not execute it; create a SmokeTestRun that
// references its name to run it. The controller uses the infra-smoketest namespace.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type SmokeTest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec defines the stages to execute and the assertions required to pass.
	Spec SmokeTestSpec `json:"spec"`
}

// SmokeTestSpec defines the stages of a smoke test. Each Run snapshots the
// definition when accepted, so later edits do not change an accepted Run.
type SmokeTestSpec struct {
	// Stages contains 1 to 16 uniquely named stages. Dependencies must form an
	// acyclic graph. The controller executes one ready stage at a time, choosing
	// stages in list order after their dependencies have succeeded.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Stages []Stage `json:"stages"`
}

// Stage performs exactly one action: create a Pod, create a PVC, delete a
// previously created resource, or wait for a set of assertions to pass.
type Stage struct {
	// Name uniquely identifies this stage within the definition and must be a
	// Kubernetes DNS label. Other stages refer to it through dependsOn and fromStage.
	Name string `json:"name"`
	// DependsOn lists stages that must succeed before this stage can start.
	// Omit it for a stage with no prerequisites.
	DependsOn []string `json:"dependsOn,omitempty"`
	// Timeout is the maximum time allowed after the stage starts, expressed as a
	// Go duration such as "30s" or "5m". It defaults to "5m", must be greater than
	// zero and at most "25m", and cannot extend the Run's execution deadline.
	// +kubebuilder:default="5m"
	Timeout string `json:"timeout,omitempty"`
	// CreatePod creates a Run-owned probe Pod from a native Kubernetes template.
	// Completion of this action means the Pod was created; use assertions in a
	// dependent stage to wait for readiness, scheduling, or probe results.
	CreatePod *PodAction `json:"createPod,omitempty"`
	// CreatePVC creates a Run-owned PersistentVolumeClaim from a native Kubernetes
	// template. Use a dependent assertion stage to wait for binding or attachment.
	CreatePVC *PVCAction `json:"createPVC,omitempty"`
	// DeleteResource deletes a Pod or PVC created by an ancestor stage and waits
	// for that object's UID to disappear. The reference must omit relation;
	// related Nodes, PVs, and VolumeAttachments cannot be deleted by this action.
	DeleteResource *ResourceRef `json:"deleteResource,omitempty"`
	// Assert contains 1 to 16 checks that must all pass for this stage to succeed.
	// Unmet checks are retried until the stage deadline or a definitive failure.
	Assert []Assertion `json:"assert,omitempty"`
}

// PodAction creates a probe Pod in the Run's namespace.
type PodAction struct {
	// Template uses native Pod metadata and spec fields, including nodeSelector,
	// tolerations, containers, and volumes. The supported subset requires
	// restartPolicy Never and one container named "probe", with command
	// ["/bin/infra-smoketest"] and args ["probe", "ready"], ["probe", "storage-write"],
	// ["probe", "storage-read"], ["probe", "identity"], ["probe", "cert-manager"],
	// or ["probe", "kube-state-metrics"]. Omit the container image
	// to inherit the Run's approved probe image, which defaults to the controller
	// image; an explicit image must match it. Use serviceAccountName
	// "infra-smoketest-probe", or "infra-smoketest-aws" for the identity probe.
	// The cert-manager probe uses "infra-smoketest-cert-manager" and accepts
	// literal CERT_MANAGER_ISSUER_NAME and CERT_MANAGER_ISSUER_KIND environment
	// values to use an existing cert-manager.io Issuer or ClusterIssuer (kind
	// defaults to Issuer). Omit both to create a temporary SelfSigned Issuer.
	// Existing namespaced Issuers must be in the Run's namespace. The metrics
	// probe accepts a literal KUBE_STATE_METRICS_URL environment value.
	// The identity probe also requires metadata.labels.pod-identity-webhook:
	// "required". PVC claimName values refer to logical PVC template names from
	// ancestor stages. The controller assigns the Pod's actual name and ownership
	// and enforces non-root execution with API token automount disabled.
	Template corev1.PodTemplateSpec `json:"template"`
}

// PVCAction creates a fresh PersistentVolumeClaim in the Run's namespace.
type PVCAction struct {
	// Template uses native PVC fields. Set metadata.name to a unique logical
	// name that Pod volumes can use as claimName; the controller substitutes a
	// Run-specific name. The supported claim requests 1Gi with ReadWriteOnce
	// access, Filesystem volumeMode, and storageClassName "ebs-gp3". Omit
	// volumeMode to use Filesystem. Existing volumes and data sources are not
	// supported because the test must validate fresh provisioning.
	Template corev1.PersistentVolumeClaimTemplate `json:"template"`
}

// ResourceRef identifies a resource created by an ancestor stage, or a related
// resource observed by the controller. References are scoped to this Run.
type ResourceRef struct {
	// FromStage names the stage that created the Pod or PVC. It must be a direct
	// or transitive dependency of the stage using this reference.
	FromStage string `json:"fromStage"`
	// Relation selects a related resource. Omit it to select the created Pod or
	// PVC itself. Use scheduledNode for a Pod's Node, boundPV for a PVC's
	// PersistentVolume, or volumeAttachment for a PVC's observed VolumeAttachment.
	// +kubebuilder:validation:Enum="";scheduledNode;boundPV;volumeAttachment
	Relation string `json:"relation,omitempty"`
}

// Assertion checks cluster state or recorded evidence for this Run.
type Assertion struct {
	// Type selects the check to perform:
	// ResourceStatus matches the resource's native status.phase and conditions.
	// NodePoolEmpty waits for zero Nodes matching the smoke-test nodeSelector,
	// including NotReady Nodes, and records a cluster Node UID baseline.
	// InitiallyUnschedulable requires recorded unschedulable evidence for a Pod.
	// AutoscalerScaleUpObserved requires a cluster-autoscaler TriggeredScaleUp
	// Event referring to that Pod's UID.
	// ScheduledOnNewNode uses relation scheduledNode and requires a new,
	// initialized AWS smoke-test Node created after the Pod and absent from the baseline.
	// VolumeBound requires a PVC bound to a freshly provisioned EBS CSI volume
	// with Delete reclaim policy.
	// VolumeAttached requires recorded EBS CSI attachment to the Node hosting a
	// Run Pod that uses the PVC.
	// WebIdentity checks the Pod's injected AWS environment and projected token.
	// ProbeResult requires a successful probe result, including the expected
	// storage checksum or AWS caller identity when applicable.
	// +kubebuilder:validation:Enum=ResourceStatus;NodePoolEmpty;InitiallyUnschedulable;AutoscalerScaleUpObserved;ScheduledOnNewNode;VolumeBound;VolumeAttached;WebIdentity;ProbeResult
	Type string `json:"type"`
	// Resource identifies the Run resource to check. It is required for every
	// assertion type except NodePoolEmpty, which uses nodeSelector instead.
	Resource *ResourceRef `json:"resource,omitempty"`
	// NodeSelector contains the exact-match Node labels for NodePoolEmpty.
	// It must be {"turnkey.engineering/designated-for": "smoke-tests"}.
	// Pod scheduling is configured separately in createPod.template.spec.nodeSelector.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Status specifies expected native resource status for ResourceStatus.
	// At least one of phase or conditions is required for that assertion type.
	Status *ExpectedStatus `json:"status,omitempty"`
}

// ExpectedStatus describes native resource status fields to match without
// changing the resource. All specified fields must match.
type ExpectedStatus struct {
	// Phase is the expected value of status.phase, such as Running for a Pod or
	// Bound for a PVC. An omitted phase is not checked.
	Phase string `json:"phase,omitempty"`
	// Conditions lists expected entries in status.conditions, matched by type.
	// Each listed condition must match; additional conditions are ignored.
	Conditions []ExpectedCondition `json:"conditions,omitempty"`
}

// ExpectedCondition matches one entry in a resource's native status.conditions.
type ExpectedCondition struct {
	// Type names the condition to match, such as Ready or PodScheduled.
	Type string `json:"type"`
	// Status is the required condition value: "True", "False", or "Unknown".
	// +kubebuilder:validation:Enum=True;False;Unknown
	Status string `json:"status"`
	// Reason optionally requires an exact condition reason, such as Unschedulable.
	// Omit it to match any reason.
	Reason string `json:"reason,omitempty"`
}

// SmokeTestList contains SmokeTest definitions.
// +kubebuilder:object:root=true
type SmokeTestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items contains the SmokeTest definitions returned by the list operation.
	Items []SmokeTest `json:"items"`
}

// SmokeTestRun requests one execution of a named SmokeTest in the same namespace.
// The controller snapshots the definition, executes its stages, records evidence,
// and cleans up Run-owned resources. Create a new Run to retry; spec is immutable.
// Runs are processed in the infra-smoketest namespace.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.reason"
// +kubebuilder:printcolumn:name="FailedStage",type=string,JSONPath=".status.stages[?(@.phase=='Failed')].name",description="Stage recorded as Failed; empty when no stage failed"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1,description="Detailed explanation of the Run outcome"
type SmokeTestRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec selects the SmokeTest definition and optional context for this execution.
	// It cannot be changed after creation; create a new Run to retry.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Run spec is immutable; create a new Run to retry"
	Spec SmokeTestRunSpec `json:"spec"`
	// Status records the accepted definition, execution progress, observed
	// resources, probe results, and cleanup outcome. It is managed by the controller.
	Status SmokeTestRunStatus `json:"status,omitempty"`
}

// SmokeTestRunSpec selects a reusable SmokeTest for a single execution.
type SmokeTestRunSpec struct {
	// TestRef references an existing SmokeTest in the same namespace. List
	// available definitions with "kubectl get smoketests -n infra-smoketest"
	// and set testRef.name to one of their names. The controller snapshots the
	// selected definition when accepting the Run.
	TestRef TestReference `json:"testRef"`
	// Context holds optional caller-supplied metadata, such as trigger or
	// deploymentRevision. These values describe why the Run was created; they
	// do not parameterize stages or verify the deployed revision.
	Context map[string]string `json:"context,omitempty"`
}

// TestReference selects a SmokeTest by name, optionally requiring a specific
// UID or generation. A mismatch causes the Run to fail with PreconditionFailed.
type TestReference struct {
	// Name is the metadata.name of a SmokeTest in the Run's namespace.
	// The example definitions include cluster-autoscaler, ebs-csi,
	// aws-pod-identity-webhook, cert-manager, and kube-state-metrics. List them with
	// "kubectl get smoketests -n infra-smoketest".
	Name string `json:"name"`
	// UID optionally requires the SmokeTest's metadata.uid to match, detecting
	// deletion and recreation of a definition with the same name.
	UID string `json:"uid,omitempty"`
	// Generation optionally requires the SmokeTest's metadata.generation to
	// match. Omit it or set it to zero to accept the current generation.
	Generation int64 `json:"generation,omitempty"`
}

// SmokeTestRunStatus records the definition snapshot and execution evidence.
// A Run succeeds only after all stages pass and cleanup completes.
type SmokeTestRunStatus struct {
	// Phase summarizes the lifecycle: Pending, Running, CleaningUp, Succeeded,
	// Failed, or Cancelled. Check conditions for the cleanup outcome, even when
	// the phase is terminal.
	Phase string `json:"phase,omitempty"`
	// Reason is a machine-readable explanation of the outcome, such as
	// AssertionsPassed, PreconditionFailed, TimedOut, AssertionFailed,
	// ControllerError, CleanupFailed, or Cancelled.
	Reason string `json:"reason,omitempty"`
	// Message is a human-readable explanation of the outcome or failure.
	Message string `json:"message,omitempty"`
	// Definition is the SmokeTest spec snapshot accepted for this Run. Later
	// edits to the source SmokeTest do not change this execution.
	Definition *SmokeTestSpec `json:"definition,omitempty"`
	// DefinitionUID is the metadata.uid of the snapshotted SmokeTest.
	DefinitionUID string `json:"definitionUID,omitempty"`
	// DefinitionGeneration is the metadata.generation of the snapshotted SmokeTest.
	DefinitionGeneration int64 `json:"definitionGeneration,omitempty"`
	// DefinitionHash is the hexadecimal SHA-256 digest of the JSON-encoded
	// definition snapshot, used to identify the exact spec executed.
	DefinitionHash string `json:"definitionHash,omitempty"`
	// ProbeImage is the approved image saved when the Run is accepted. Omitted
	// Pod template images inherit this value. It defaults to the controller's
	// image unless the controller is configured with --probe-image.
	ProbeImage string `json:"probeImage,omitempty"`
	// AWSAccountID is the AWS account ID saved when the Run is accepted. It is
	// the account of ExpectedRoleARN. Every probe receives it as
	// SMOKETEST_AWS_ACCOUNT_ID. Later changes to the ServiceAccount annotation
	// or controller configuration do not affect Runs that have already been accepted.
	// +kubebuilder:validation:Pattern=`^[0-9]{12}$`
	AWSAccountID string `json:"awsAccountID,omitempty"`
	// ExpectedRoleARN is the expected IAM role ARN for webhook and AWS caller
	// identity checks. It is read from the identity ServiceAccount's
	// eks.amazonaws.com/role-arn annotation when the Run is accepted, unless the
	// controller pins it with --expected-role-arn or --aws-account-id.
	ExpectedRoleARN string `json:"expectedRoleARN,omitempty"`
	// Region is the AWS region saved from controller configuration for identity probes.
	Region string `json:"region,omitempty"`
	// StartedAt is when the Run acquired the execution slot and began execution.
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// Deadline is the overall execution deadline, 25 minutes after startedAt.
	// Cleanup has a separate deadline.
	Deadline *metav1.Time `json:"deadline,omitempty"`
	// CleanupStartedAt is when cleanup began after completion, failure, or
	// cancellation. Cleanup exceeding five minutes records CleanupFailed;
	// the controller continues retrying while retaining the finalizer.
	CleanupStartedAt *metav1.Time `json:"cleanupStartedAt,omitempty"`
	// CompletedAt is when cleanup completed and the final outcome was recorded.
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// Stages records progress for stages that have started, keyed by stage name.
	// +listType=map
	// +listMapKey=name
	Stages []StageStatus `json:"stages,omitempty"`
	// Resources tracks Pods and PVCs created by this Run, along with related
	// cluster resources and recorded evidence used by assertions and cleanup.
	Resources []ResourceRecord `json:"resources,omitempty"`
	// BaselineNodes contains the UIDs of all cluster Nodes recorded when
	// NodePoolEmpty passed. ScheduledOnNewNode uses it to exclude existing capacity.
	BaselineNodes []string `json:"baselineNodes,omitempty"`
	// BaselineAt is when the Node UID baseline was recorded.
	BaselineAt *metav1.Time `json:"baselineAt,omitempty"`
	// Conditions reports Accepted, Complete, Succeeded, and CleanupComplete.
	// Complete alone does not mean the test passed or cleanup finished;
	// inspect Succeeded and CleanupComplete as well.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// StageStatus records execution progress for one named stage.
type StageStatus struct {
	// Name identifies the stage in the saved definition.
	Name string `json:"name"`
	// Phase is the stage's execution state: Running, Succeeded, or Failed.
	Phase string `json:"phase"`
	// StartedAt is when execution of this stage began.
	StartedAt metav1.Time `json:"startedAt"`
	// Deadline is the stage timeout measured from startedAt, capped by the Run deadline.
	Deadline metav1.Time `json:"deadline"`
	// CompletedAt is when the stage succeeded. It may be absent for a failed stage.
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// Message describes the latest action or the evidence the stage is waiting for.
	Message string `json:"message,omitempty"`
}

// ResourceRecord tracks a Run-owned Pod or PVC and its observed relationships.
// Related Nodes, PVs, and VolumeAttachments are recorded for verification and
// cleanup observation; they are not owned by the Run.
type ResourceRecord struct {
	// Stage names the stage that creates this resource.
	Stage string `json:"stage"`
	// Kind is the created resource kind: Pod or PersistentVolumeClaim.
	Kind string `json:"kind"`
	// Name is the actual Kubernetes resource name assigned by the controller.
	Name string `json:"name"`
	// UID is the created object's metadata.uid, used to detect replacement and
	// correlate observations with the exact resource created by this Run.
	UID string `json:"uid,omitempty"`
	// CreatedAt is the resource's metadata.creationTimestamp.
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`
	// Deleted records that a stage requested deletion and disappearance is
	// expected. It does not by itself confirm that cleanup has completed.
	Deleted bool `json:"deleted,omitempty"`
	// UnschedulableAt is the timestamp of recorded PodScheduled=False with
	// reason Unschedulable, or a FailedScheduling Event for this Pod.
	UnschedulableAt *metav1.Time `json:"unschedulableAt,omitempty"`
	// ScaleUpAt is the timestamp of a recorded cluster-autoscaler
	// TriggeredScaleUp Event for this Pod's UID.
	ScaleUpAt *metav1.Time `json:"scaleUpAt,omitempty"`
	// NodeName is the Node observed hosting this Pod.
	NodeName string `json:"nodeName,omitempty"`
	// NodeUID is the metadata.uid of the observed Node.
	NodeUID string `json:"nodeUID,omitempty"`
	// PVName is the PersistentVolume observed bound to this PVC.
	PVName string `json:"pvName,omitempty"`
	// PVUID is the metadata.uid of the bound PersistentVolume.
	PVUID string `json:"pvUID,omitempty"`
	// VolumeHandle is the CSI volume handle recorded from the bound PV,
	// expected to be an EBS volume ID for EBS tests.
	VolumeHandle string `json:"volumeHandle,omitempty"`
	// AttachmentName is the VolumeAttachment observed for this PVC's bound volume.
	AttachmentName string `json:"attachmentName,omitempty"`
	// AttachmentUID is the metadata.uid of the observed VolumeAttachment.
	AttachmentUID string `json:"attachmentUID,omitempty"`
	// Attached records that an attached EBS VolumeAttachment and EBS CSI Node
	// registration were observed. This evidence is retained after detachment.
	Attached bool `json:"attached,omitempty"`
	// Result is the structured result collected from the probe container's
	// termination message and correlated with this Run and stage.
	Result *ProbeResult `json:"result,omitempty"`
}

// ProbeResult is the structured termination result reported by a finite probe.
type ProbeResult struct {
	// Version identifies the probe result format. The supported version is 1.
	Version int `json:"version"`
	// RunUID is the metadata.uid of the SmokeTestRun that invoked the probe.
	RunUID string `json:"runUID"`
	// Stage identifies the stage that created the probe Pod.
	Stage string `json:"stage"`
	// Container is the reporting container name, which must be "probe".
	Container string `json:"container"`
	// Success reports whether the probe completed successfully. The controller
	// also requires a zero container exit code and validates the result's evidence.
	Success bool `json:"success"`
	// Checksum is the hexadecimal SHA-256 checksum of the Run-specific sentinel
	// written or read by a storage probe.
	Checksum string `json:"checksum,omitempty"`
	// Account is the AWS account ID returned by STS GetCallerIdentity for an identity probe.
	Account string `json:"account,omitempty"`
	// ARN is the STS caller ARN returned by an identity probe, checked against
	// the expected IAM role. An assumed-role caller ARN differs from the IAM role ARN.
	ARN string `json:"arn,omitempty"`
	// CertificateSHA256 is the SHA-256 fingerprint of the verified certificate
	// issued by cert-manager for this probe's fresh private key and DNS name.
	CertificateSHA256 string `json:"certificateSHA256,omitempty"`
	// MetricsPodUID identifies the probe Pod observed in kube-state-metrics
	// kube_pod_info and Running kube_pod_status_phase samples.
	MetricsPodUID string `json:"metricsPodUID,omitempty"`
	// Error is a bounded failure message reported by the probe.
	Error string `json:"error,omitempty"`
}

// SmokeTestRunList contains SmokeTestRun executions.
// +kubebuilder:object:root=true
type SmokeTestRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items contains the SmokeTestRun executions returned by the list operation.
	Items []SmokeTestRun `json:"items"`
}
