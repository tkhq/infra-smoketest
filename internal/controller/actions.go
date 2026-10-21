package controller

import (
	"context"
	"fmt"
	"slices"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"github.com/tkhq/infra-smoketest/internal/definition"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *Reconciler) step(ctx context.Context, run *api.SmokeTestRun, stage api.Stage) (bool, string, error) {
	ctx = ctrl.LoggerInto(ctx, ctrl.LoggerFrom(ctx).WithValues("stage", stage.Name))
	if stage.CreatePod != nil || stage.CreatePVC != nil {
		rec := record(run, stage.Name)
		if rec == nil {
			kind := "Pod"
			if stage.CreatePVC != nil {
				kind = "PersistentVolumeClaim"
			}
			run.Status.Resources = append(run.Status.Resources, api.ResourceRecord{Stage: stage.Name, Kind: kind, Name: name(run, stage.Name)})
			return false, "Resource intent saved", nil
		}
		var obj client.Object
		if stage.CreatePod != nil {
			p, err := r.pod(ctx, run, stage, rec.Name)
			if err != nil {
				return false, "", err
			}
			obj = p
		} else {
			if err := r.storagePreflight(ctx); err != nil {
				return false, "", err
			}
			obj = &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: rec.Name, Namespace: run.Namespace, Labels: map[string]string{RunLabel: string(run.UID)}, OwnerReferences: owner(run)}, Spec: *stage.CreatePVC.Template.Spec.DeepCopy()}
		}
		existing := obj.DeepCopyObject().(client.Object)
		err := r.Get(ctx, key(rec.Name), existing)
		if apierrors.IsNotFound(err) {
			if rec.UID != "" {
				return false, "", fail("AssertionFailed", "%s was deleted externally", rec.Name)
			}
			if err = r.Create(ctx, obj); err != nil {
				if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
					return false, "", fail("PreconditionFailed", "admission rejected %s: %v", rec.Name, err)
				}
				return false, "", err
			}
			existing = obj
			ctrl.LoggerFrom(ctx).Info("Created resource", "resourceKind", rec.Kind, "resource", client.ObjectKeyFromObject(obj).String(), "resourceUID", obj.GetUID())
		} else if err != nil {
			return false, "", err
		}
		if !owned(existing, run) || (rec.UID != "" && rec.UID != string(existing.GetUID())) {
			return false, "", fail("AssertionFailed", "resource name/UID collision: %s", rec.Name)
		}
		rec.UID = string(existing.GetUID())
		t := existing.GetCreationTimestamp()
		rec.CreatedAt = &t
		return true, "Created " + rec.Name, nil
	}
	if stage.DeleteResource != nil {
		rec := record(run, stage.DeleteResource.FromStage)
		if rec == nil {
			return false, "", fail("ControllerError", "missing resource record")
		}
		var obj client.Object = &corev1.Pod{}
		if rec.Kind == "PersistentVolumeClaim" {
			obj = &corev1.PersistentVolumeClaim{}
		}
		err := r.Get(ctx, key(rec.Name), obj)
		if apierrors.IsNotFound(err) {
			rec.Deleted = true
			return true, "Resource removed", nil
		}
		if err != nil {
			return false, "", err
		}
		if !owned(obj, run) || string(obj.GetUID()) != rec.UID {
			return false, "", fail("AssertionFailed", "resource identity changed before deletion")
		}
		// Save deletion intent before issuing Delete so disappearance is expected
		// even if a controller restart occurs before the stage completes.
		if !rec.Deleted {
			rec.Deleted = true
			return false, "Deletion intent saved", nil
		}
		if obj.GetDeletionTimestamp().IsZero() {
			err = r.deleteResource(ctx, obj, rec.Kind)
		}
		return false, "Waiting for resource deletion", client.IgnoreNotFound(err)
	}
	for _, a := range stage.Assert {
		done, message, err := r.assert(ctx, run, a)
		if err != nil || !done {
			return done, fmt.Sprintf("%s: %s", a.Type, message), err
		}
	}
	return true, "Assertions passed", nil
}

func (r *Reconciler) deleteResource(ctx context.Context, obj client.Object, kind string) error {
	if err := r.Delete(ctx, obj, client.Preconditions{UID: ptr.To(obj.GetUID())}); err != nil {
		return client.IgnoreNotFound(err)
	}
	ctrl.LoggerFrom(ctx).Info("Requested resource deletion", "resourceKind", kind, "resource", client.ObjectKeyFromObject(obj).String(), "resourceUID", obj.GetUID())
	return nil
}

func (r *Reconciler) storagePreflight(ctx context.Context) error {
	sc := &storagev1.StorageClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: "ebs-gp3"}, sc); err != nil {
		if apierrors.IsNotFound(err) {
			return fail("PreconditionFailed", "StorageClass ebs-gp3 is missing")
		}
		return err
	}
	if sc.Provisioner != "ebs.csi.aws.com" || sc.VolumeBindingMode == nil || *sc.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer || sc.Parameters["type"] != "gp3" || sc.Parameters["encrypted"] != "true" || sc.ReclaimPolicy == nil || *sc.ReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		return fail("PreconditionFailed", "ebs-gp3 must use EBS CSI, WaitForFirstConsumer, gp3, encryption, and Delete reclaim policy")
	}
	return nil
}

func (r *Reconciler) pod(ctx context.Context, run *api.SmokeTestRun, stage api.Stage, podName string) (*corev1.Pod, error) {
	t := stage.CreatePod.Template.DeepCopy()
	spec := t.Spec.DeepCopy()
	c := &spec.Containers[0]
	sa := &corev1.ServiceAccount{}
	if err := r.Get(ctx, key(spec.ServiceAccountName), sa); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fail("PreconditionFailed", "ServiceAccount %s is missing", spec.ServiceAccountName)
		}
		return nil, err
	}
	if c.Args[1] == "identity" && run.Status.ExpectedRoleARN == "" {
		return nil, fail("PreconditionFailed", "no expected IAM role: annotate the %s ServiceAccount with a valid %s or pin one with --expected-role-arn", definition.IdentitySA, RoleAnnotation)
	}
	if c.Args[1] == "identity" && sa.Annotations[RoleAnnotation] != run.Status.ExpectedRoleARN {
		return nil, fail("PreconditionFailed", "%s ServiceAccount role annotation does not match the Run's expected role; expected %q, found %q", definition.IdentitySA, run.Status.ExpectedRoleARN, sa.Annotations[RoleAnnotation])
	}
	if c.Args[1] != "identity" && sa.Annotations[RoleAnnotation] != "" {
		return nil, fail("PreconditionFailed", "ordinary probe ServiceAccount must not have an IAM role")
	}
	c.Image = run.Status.ProbeImage
	c.Env = append(c.Env, []corev1.EnvVar{{Name: "SMOKETEST_RUN_UID", Value: string(run.UID)}, {Name: "SMOKETEST_STAGE", Value: stage.Name}, {Name: "SMOKETEST_AWS_ACCOUNT_ID", Value: run.Status.AWSAccountID}, {Name: "SMOKETEST_EXPECTED_ROLE_ARN", Value: run.Status.ExpectedRoleARN}, {Name: "SMOKETEST_REGION", Value: run.Status.Region}, {Name: "AWS_EC2_METADATA_DISABLED", Value: "true"}}...)
	for _, field := range []struct{ env, path string }{{"SMOKETEST_POD_NAME", "metadata.name"}, {"SMOKETEST_POD_NAMESPACE", "metadata.namespace"}, {"SMOKETEST_POD_UID", "metadata.uid"}} {
		c.Env = append(c.Env, corev1.EnvVar{Name: field.env, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: field.path}}})
	}
	c.TerminationMessagePath = "/dev/termination-log"
	c.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	c.SecurityContext = &corev1.SecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532)), ReadOnlyRootFilesystem: ptr.To(true), AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532)), FSGroup: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	spec.AutomountServiceAccountToken = ptr.To(false)
	seconds := int64(run.Status.Deadline.Sub(r.now()).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	spec.ActiveDeadlineSeconds = &seconds
	if c.Resources.Requests == nil {
		c.Resources.Requests = corev1.ResourceList{}
	}
	if c.Resources.Limits == nil {
		c.Resources.Limits = corev1.ResourceList{}
	}
	for k, v := range map[corev1.ResourceName]string{corev1.ResourceCPU: "100m", corev1.ResourceMemory: "64Mi"} {
		if _, ok := c.Resources.Requests[k]; !ok {
			c.Resources.Requests[k] = resource.MustParse(v)
		}
	}
	for k, v := range map[corev1.ResourceName]string{corev1.ResourceCPU: "250m", corev1.ResourceMemory: "128Mi"} {
		if _, ok := c.Resources.Limits[k]; !ok {
			c.Resources.Limits[k] = resource.MustParse(v)
		}
	}
	if spec.NodeSelector == nil {
		spec.NodeSelector = map[string]string{}
	}
	if c.Args[1] == "ready" {
		if spec.NodeSelector[definition.PoolLabel] != definition.PoolValue {
			return nil, fail("PreconditionFailed", "autoscaler probe requires the smoke nodeSelector")
		}
	} else {
		if spec.NodeSelector[definition.PoolLabel] == definition.PoolValue {
			return nil, fail("PreconditionFailed", "non-autoscaler probes must exclude the smoke pool")
		}
		spec.NodeSelector["turnkey.engineering/cloud-platform"] = "aws"
		if spec.Affinity == nil {
			spec.Affinity = &corev1.Affinity{}
		}
		if spec.Affinity.NodeAffinity == nil {
			spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
		}
		n := spec.Affinity.NodeAffinity
		if n.RequiredDuringSchedulingIgnoredDuringExecution == nil {
			n.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{}}}
		}
		for i := range n.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			term := &n.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[i]
			term.MatchExpressions = append(term.MatchExpressions, corev1.NodeSelectorRequirement{Key: definition.PoolLabel, Operator: corev1.NodeSelectorOpNotIn, Values: []string{definition.PoolValue}})
		}
	}
	for i := range spec.Volumes {
		logical := spec.Volumes[i].PersistentVolumeClaim.ClaimName
		found := false
		for _, s := range run.Status.Definition.Stages {
			if s.CreatePVC != nil && s.CreatePVC.Template.Name == logical {
				rec := record(run, s.Name)
				if rec == nil {
					return nil, fail("ControllerError", "PVC record missing")
				}
				spec.Volumes[i].PersistentVolumeClaim.ClaimName = rec.Name
				found = true
			}
		}
		if !found {
			return nil, fail("ControllerError", "unresolved PVC reference")
		}
		if c.Args[1] == "storage-read" {
			for _, s := range run.Status.Definition.Stages {
				if s.CreatePod == nil || !slices.Equal(s.CreatePod.Template.Spec.Containers[0].Args, []string{"probe", "storage-write"}) {
					continue
				}
				writer := record(run, s.Name)
				if writer == nil || writer.NodeName == "" {
					continue
				}
				if len(s.CreatePod.Template.Spec.Volumes) == 0 || s.CreatePod.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != logical {
					continue
				}
				node := &corev1.Node{}
				if err := r.Get(ctx, types.NamespacedName{Name: writer.NodeName}, node); err != nil {
					return nil, err
				}
				if string(node.UID) != writer.NodeUID {
					return nil, fail("AssertionFailed", "writer node was replaced")
				}
				hostname := node.Labels["kubernetes.io/hostname"]
				if hostname == "" {
					return nil, fmt.Errorf("writer node lacks hostname label")
				}
				spec.NodeSelector["kubernetes.io/hostname"] = hostname
				reader := record(run, stage.Name)
				reader.NodeName, reader.NodeUID = writer.NodeName, writer.NodeUID
			}
			if spec.NodeSelector["kubernetes.io/hostname"] == "" {
				return nil, fail("PreconditionFailed", "reader requires a completed writer on the same PVC")
			}
		}
	}
	if c.Args[1] == "cert-manager" {
		// Only this probe receives a Kubernetes API token. The mounted token is
		// short-lived and bound to the Pod; templates cannot supply projections.
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "kube-api-access", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To(int32(0444)), Sources: []corev1.VolumeProjection{
			{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: ptr.To(int64(600))}},
			{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
			{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
		}}}})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "kube-api-access", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true})
	}
	if t.Labels == nil {
		t.Labels = map[string]string{}
	}
	t.Labels[RunLabel] = string(run.UID)
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: run.Namespace, Labels: t.Labels, OwnerReferences: owner(run)}, Spec: *spec}, nil
}
