// Package controller reconciles durable, bounded smoke-test Runs.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"github.com/tkhq/infra-smoketest/internal/definition"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const Finalizer = "smoketest.turnkey.engineering/cleanup"
const RunLabel = "smoketest.turnkey.engineering/run-uid"
const LeaseName = "infra-smoketest-execution"

type Reconciler struct {
	client.Client   // Direct API client: status and lock decisions must not use stale cache reads.
	ProbeImage      string
	AWSAccountID    string
	ExpectedRoleARN string
	Region          string
	Now             func() time.Time
}

type failure struct{ reason, message string }

func (f *failure) Error() string { return f.message }
func fail(reason, format string, args ...any) error {
	return &failure{reason, fmt.Sprintf(format, args...)}
}
func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
func key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: definition.Namespace, Name: name}
}
func owned(obj client.Object, run *api.SmokeTestRun) bool {
	for _, owner := range obj.GetOwnerReferences() {
		if owner.UID == run.UID && owner.Kind == "SmokeTestRun" && owner.APIVersion == api.GroupVersion.String() {
			return true
		}
	}
	return false
}
func name(run *api.SmokeTestRun, stage string) string {
	sum := sha256.Sum256([]byte(string(run.UID) + "/" + stage))
	return "smoke-" + hex.EncodeToString(sum[:10])
}
func owner(run *api.SmokeTestRun) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SmokeTestRun", Name: run.Name, UID: run.UID, Controller: ptr.To(true)}}
}
func condition(run *api.SmokeTestRun, t string, yes bool, reason, message string, now time.Time) {
	status := metav1.ConditionFalse
	if yes {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{Type: t, Status: status, Reason: reason, Message: message, ObservedGeneration: run.Generation, LastTransitionTime: metav1.NewTime(now)})
}
func (r *Reconciler) save(ctx context.Context, run *api.SmokeTestRun, before *api.SmokeTestRunStatus) error {
	if reflect.DeepEqual(*before, run.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, run); err != nil {
		return err
	}
	logStatusChanges(ctx, before, &run.Status)
	return nil
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Pod/PVC changes enqueue their owner. Events and cluster-scoped changes
	// enqueue the single active Run. A periodic reconcile also recovers missed watches.
	active := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		lease := &coordv1.Lease{}
		if r.Get(ctx, key(LeaseName), lease) != nil || lease.Spec.HolderIdentity == nil {
			return nil
		}
		list := &api.SmokeTestRunList{}
		if r.List(ctx, list, client.InNamespace(definition.Namespace)) != nil {
			return nil
		}
		for _, run := range list.Items {
			if string(run.UID) == *lease.Spec.HolderIdentity {
				return []reconcile.Request{{NamespacedName: key(run.Name)}}
			}
		}
		return nil
	})
	return ctrl.NewControllerManagedBy(mgr).For(&api.SmokeTestRun{}).Owns(&corev1.Pod{}).Owns(&corev1.PersistentVolumeClaim{}).
		Watches(&corev1.Event{}, active).Watches(&corev1.Node{}, active).Watches(&corev1.PersistentVolume{}, active).Watches(&storagev1.VolumeAttachment{}, active).Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != definition.Namespace {
		return ctrl.Result{}, nil
	}
	run := &api.SmokeTestRun{}
	if err := r.Get(ctx, req.NamespacedName, run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ctx = ctrl.LoggerInto(ctx, ctrl.LoggerFrom(ctx).WithValues("run", req.NamespacedName.String(), "runUID", run.UID, "test", run.Spec.TestRef.Name))
	ctrl.LoggerFrom(ctx).V(1).Info("Reconciling Run", "phase", run.Status.Phase)
	now := r.now()
	before := run.Status.DeepCopy()
	if !slices.Contains(run.Finalizers, Finalizer) {
		if !run.DeletionTimestamp.IsZero() {
			return ctrl.Result{}, nil
		}
		if meta.IsStatusConditionTrue(run.Status.Conditions, "CleanupComplete") {
			if run.Status.CompletedAt != nil && now.Sub(run.Status.CompletedAt.Time) > 7*24*time.Hour {
				return ctrl.Result{}, r.Delete(ctx, run)
			}
			return ctrl.Result{RequeueAfter: time.Hour}, nil
		}
		run.Finalizers = append(run.Finalizers, Finalizer)
		return ctrl.Result{RequeueAfter: time.Millisecond}, r.Update(ctx, run)
	}
	if !run.DeletionTimestamp.IsZero() && run.Status.CleanupStartedAt == nil {
		r.finish(run, "Cancelled", "Run deletion requested", now)
	}
	if run.Status.CleanupStartedAt != nil {
		err := r.cleanup(ctx, run, now)
		if err != nil {
			if now.Sub(run.Status.CleanupStartedAt.Time) > 5*time.Minute {
				run.Status.Phase = "Failed"
				if run.Status.Reason == "AssertionsPassed" {
					run.Status.Reason = "CleanupFailed"
				}
				condition(run, "CleanupComplete", false, "CleanupFailed", "Cleanup API error; finalizer and execution slot retained", now)
				condition(run, "Complete", true, "CleanupFailed", "Cleanup deadline exceeded; retries continue", now)
			}
			if saveErr := r.save(ctx, run, before); saveErr != nil {
				return ctrl.Result{}, saveErr
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}
		if err = r.save(ctx, run, before); err != nil {
			return ctrl.Result{}, err
		}
		if meta.IsStatusConditionTrue(run.Status.Conditions, "CleanupComplete") {
			if err = r.release(ctx, run); err != nil {
				return ctrl.Result{}, err
			}
			run.Finalizers = slices.DeleteFunc(run.Finalizers, func(s string) bool { return s == Finalizer })
			return ctrl.Result{RequeueAfter: time.Hour}, r.Update(ctx, run)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if run.Status.Definition == nil {
		test := &api.SmokeTest{}
		err := r.Get(ctx, key(run.Spec.TestRef.Name), test)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err == nil {
			err = definition.Validate(test.Spec, r.ProbeImage)
		}
		if err == nil && ((run.Spec.TestRef.UID != "" && run.Spec.TestRef.UID != string(test.UID)) || (run.Spec.TestRef.Generation != 0 && run.Spec.TestRef.Generation != test.Generation)) {
			err = errors.New("definition revision does not match testRef")
		}
		if err != nil {
			r.finish(run, "PreconditionFailed", err.Error(), now)
		} else {
			run.Status.Definition = test.Spec.DeepCopy()
			run.Status.DefinitionUID = string(test.UID)
			run.Status.DefinitionGeneration = test.Generation
			b, _ := json.Marshal(test.Spec)
			sum := sha256.Sum256(b)
			run.Status.DefinitionHash = hex.EncodeToString(sum[:])
			run.Status.ProbeImage = r.ProbeImage
			run.Status.AWSAccountID, run.Status.ExpectedRoleARN, err = r.awsIdentity(ctx)
			if err != nil {
				return ctrl.Result{}, err
			}
			run.Status.Region = r.Region
			run.Status.Phase = "Pending"
			condition(run, "Accepted", true, "Accepted", "Definition snapshot saved", now)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, r.save(ctx, run, before)
	}
	locked, err := r.acquire(ctx, run, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !locked {
		ctrl.LoggerFrom(ctx).V(1).Info("Waiting for execution slot")
		if now.Sub(run.CreationTimestamp.Time) > 10*time.Minute {
			r.finish(run, "TimedOut", "Execution slot queue deadline exceeded", now)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.save(ctx, run, before)
	}
	if run.Status.StartedAt == nil {
		run.Status.StartedAt = ptr.To(metav1.NewTime(now))
		run.Status.Deadline = ptr.To(metav1.NewTime(now.Add(25 * time.Minute)))
		run.Status.Phase = "Running"
		return ctrl.Result{RequeueAfter: time.Millisecond}, r.save(ctx, run, before)
	}
	if now.After(run.Status.Deadline.Time) {
		r.finish(run, "TimedOut", "Run execution deadline exceeded", now)
		return ctrl.Result{RequeueAfter: time.Millisecond}, r.save(ctx, run, before)
	}
	if err = r.observe(ctx, run); err == nil {
		err = r.advance(ctx, run, now)
	}
	if err != nil {
		var f *failure
		if !errors.As(err, &f) {
			return ctrl.Result{}, err
		}
		r.finish(run, f.reason, f.message, now)
	}
	return ctrl.Result{RequeueAfter: time.Second}, r.save(ctx, run, before)
}

func (r *Reconciler) finish(run *api.SmokeTestRun, reason, message string, now time.Time) {
	if len(message) > 1024 {
		message = message[:1024]
	}
	run.Status.Reason = reason
	run.Status.Message = message
	run.Status.Phase = "CleaningUp"
	run.Status.CleanupStartedAt = ptr.To(metav1.NewTime(now))
	for i := range run.Status.Stages {
		if run.Status.Stages[i].Phase == "Running" {
			run.Status.Stages[i].Phase = "Failed"
			run.Status.Stages[i].Message = message
		}
	}
	condition(run, "Succeeded", false, "CleaningUp", "Waiting for cleanup", now)
}

func (r *Reconciler) acquire(ctx context.Context, run *api.SmokeTestRun, now time.Time) (bool, error) {
	l := &coordv1.Lease{}
	err := r.Get(ctx, key(LeaseName), l)
	if apierrors.IsNotFound(err) {
		l = &coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: LeaseName, Namespace: definition.Namespace}, Spec: coordv1.LeaseSpec{HolderIdentity: ptr.To(string(run.UID)), LeaseDurationSeconds: ptr.To(int32(60)), RenewTime: ptr.To(metav1.NewMicroTime(now))}}
		err = r.Create(ctx, l)
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}
		return err == nil, err
	}
	if err != nil {
		return false, err
	}
	if l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity != "" && *l.Spec.HolderIdentity != string(run.UID) {
		return false, nil
	} // Never steal a slot from an unclean Run.
	l.Spec.HolderIdentity = ptr.To(string(run.UID))
	l.Spec.RenewTime = ptr.To(metav1.NewMicroTime(now))
	err = r.Update(ctx, l)
	return err == nil, err
}
func (r *Reconciler) release(ctx context.Context, run *api.SmokeTestRun) error {
	l := &coordv1.Lease{}
	if err := r.Get(ctx, key(LeaseName), l); err != nil {
		return client.IgnoreNotFound(err)
	}
	if l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == string(run.UID) {
		l.Spec.HolderIdentity = ptr.To("")
		if err := r.Update(ctx, l); err != nil {
			return err
		}
		ctrl.LoggerFrom(ctx).Info("Released execution slot")
	}
	return nil
}

func record(run *api.SmokeTestRun, stage string) *api.ResourceRecord {
	for i := range run.Status.Resources {
		if run.Status.Resources[i].Stage == stage {
			return &run.Status.Resources[i]
		}
	}
	return nil
}
func stageStatus(run *api.SmokeTestRun, name string) *api.StageStatus {
	for i := range run.Status.Stages {
		if run.Status.Stages[i].Name == name {
			return &run.Status.Stages[i]
		}
	}
	return nil
}
func (r *Reconciler) advance(ctx context.Context, run *api.SmokeTestRun, now time.Time) error {
	all := true
	for _, stage := range run.Status.Definition.Stages {
		state := stageStatus(run, stage.Name)
		if state != nil && state.Phase == "Succeeded" {
			continue
		}
		all = false
		ready := true
		for _, dep := range stage.DependsOn {
			s := stageStatus(run, dep)
			if s == nil || s.Phase != "Succeeded" {
				ready = false
			}
		}
		if !ready {
			continue
		}
		if state == nil {
			duration, _ := definition.Timeout(stage.Timeout)
			deadline := now.Add(duration)
			if deadline.After(run.Status.Deadline.Time) {
				deadline = run.Status.Deadline.Time
			}
			run.Status.Stages = append(run.Status.Stages, api.StageStatus{Name: stage.Name, Phase: "Running", StartedAt: metav1.NewTime(now), Deadline: metav1.NewTime(deadline)})
			return nil
		}
		if now.After(state.Deadline.Time) {
			reason := "TimedOut"
			if len(stage.Assert) > 0 && stage.Assert[0].Type == "NodePoolEmpty" {
				reason = "PreconditionFailed"
			}
			return fail(reason, "stage %s deadline exceeded: %s", stage.Name, state.Message)
		}
		done, message, err := r.step(ctx, run, stage)
		state.Message = message
		if err != nil {
			return err
		}
		if done {
			state.Phase = "Succeeded"
			state.CompletedAt = ptr.To(metav1.NewTime(now))
		}
		return nil
	}
	if all {
		r.finish(run, "AssertionsPassed", "All required assertions passed", now)
	}
	return nil
}

func (r *Reconciler) cleanup(ctx context.Context, run *api.SmokeTestRun, now time.Time) error {
	ctx = ctrl.LoggerInto(ctx, ctrl.LoggerFrom(ctx).WithValues("cleanup", true))
	before := run.Status.DeepCopy()
	// Recover a successful Create whose status update was lost before cancellation.
	for i := range run.Status.Resources {
		rec := &run.Status.Resources[i]
		if rec.UID != "" {
			continue
		}
		var obj client.Object = &corev1.Pod{}
		if rec.Kind == "PersistentVolumeClaim" {
			obj = &corev1.PersistentVolumeClaim{}
		}
		err := r.Get(ctx, key(rec.Name), obj)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err == nil && owned(obj, run) {
			rec.UID = string(obj.GetUID())
			created := obj.GetCreationTimestamp()
			rec.CreatedAt = &created
		}
	}
	// Capture claim -> PV correlation before removing consumers or claims.
	if err := r.observeClaims(ctx, run); err != nil {
		return err
	}
	// Commit cleanup evidence before any deletion, so a restart cannot lose the
	// only reference to a dynamically provisioned PV after the PVC disappears.
	if !reflect.DeepEqual(*before, run.Status) {
		return nil
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(run.Namespace)); err != nil {
		return err
	}
	remaining := false
	for i := range pods.Items {
		p := &pods.Items[i]
		if owned(p, run) {
			remaining = true
			if p.DeletionTimestamp.IsZero() {
				if err := r.deleteResource(ctx, p, "Pod"); err != nil {
					return err
				}
			}
		}
	}
	if !remaining {
		claims := &corev1.PersistentVolumeClaimList{}
		if err := r.List(ctx, claims, client.InNamespace(run.Namespace)); err != nil {
			return err
		}
		for i := range claims.Items {
			p := &claims.Items[i]
			if owned(p, run) {
				remaining = true
				if p.DeletionTimestamp.IsZero() {
					if err := r.deleteResource(ctx, p, "PersistentVolumeClaim"); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, rec := range run.Status.Resources {
		if rec.Kind != "PersistentVolumeClaim" {
			continue
		}
		if rec.PVName != "" {
			pv := &corev1.PersistentVolume{}
			err := r.Get(ctx, types.NamespacedName{Name: rec.PVName}, pv)
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			if err == nil && string(pv.UID) == rec.PVUID {
				remaining = true
			}
		}
		if rec.AttachmentName != "" {
			va := &storagev1.VolumeAttachment{}
			err := r.Get(ctx, types.NamespacedName{Name: rec.AttachmentName}, va)
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			if err == nil && string(va.UID) == rec.AttachmentUID {
				remaining = true
			}
		}
	}
	if !remaining {
		var err error
		remaining, err = r.cleanupCertificates(ctx, run)
		if err != nil {
			return err
		}
	}
	if remaining {
		ctrl.LoggerFrom(ctx).V(1).Info("Waiting for cleanup", "cleanupStartedAt", run.Status.CleanupStartedAt)
		if now.Sub(run.Status.CleanupStartedAt.Time) > 5*time.Minute {
			run.Status.Phase = "Failed"
			if run.Status.Reason == "AssertionsPassed" {
				run.Status.Reason = "CleanupFailed"
			}
			condition(run, "CleanupComplete", false, "CleanupFailed", "Owned resources or CSI dependents remain; execution slot retained", now)
			condition(run, "Complete", true, "CleanupFailed", "Cleanup deadline exceeded; cleanup continues", now)
		}
		return nil
	}
	success := run.Status.Reason == "AssertionsPassed" && run.Status.Phase != "Failed"
	run.Status.Phase = "Failed"
	if success {
		run.Status.Phase = "Succeeded"
	}
	if run.Status.Reason == "Cancelled" {
		run.Status.Phase = "Cancelled"
	}
	run.Status.CompletedAt = ptr.To(metav1.NewTime(now))
	condition(run, "CleanupComplete", true, "Cleaned", "Test resources removed", now)
	condition(run, "Complete", true, run.Status.Reason, run.Status.Message, now)
	condition(run, "Succeeded", success, run.Status.Reason, run.Status.Message, now)
	// Keep a bounded diagnostic artifact including the final result.
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name(run, "diagnostics"), Namespace: run.Namespace, OwnerReferences: owner(run), Labels: map[string]string{RunLabel: string(run.UID)}}}
	evidence := run.Status.DeepCopy()
	evidence.Definition = nil
	b, _ := json.Marshal(evidence)
	if len(b) > 128*1024 {
		b = []byte(`{"error":"diagnostic size limit exceeded"}`)
	}
	cm.Data = map[string]string{"result.json": string(b)}
	if err := r.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		existing := &corev1.ConfigMap{}
		if err = r.Get(ctx, key(cm.Name), existing); err != nil {
			return err
		}
		if !owned(existing, run) {
			return fail("ControllerError", "diagnostic ConfigMap name collision")
		}
	} else {
		ctrl.LoggerFrom(ctx).Info("Created diagnostic ConfigMap", "resource", client.ObjectKeyFromObject(cm).String())
	}
	return nil
}
