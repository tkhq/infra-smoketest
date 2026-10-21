package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"github.com/tkhq/infra-smoketest/internal/definition"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestResolveAWSIdentity(t *testing.T) {
	const account = "123456789012"
	const role = "arn:aws:iam::123456789012:role/infra-smoketest-aws"
	for _, tc := range []struct {
		name, account, roleName, roleARN, wantAccount, wantRole, wantError string
	}{
		{name: "unconfigured"},
		{name: "shared account", account: account, wantAccount: account, wantRole: role},
		{name: "named role", account: account, roleName: "test-role", wantAccount: account, wantRole: "arn:aws:iam::123456789012:role/test-role"},
		{name: "legacy ARN", roleARN: role, wantAccount: account, wantRole: role},
		{name: "explicit matching settings", account: account, roleName: "infra-smoketest-aws", roleARN: role, wantAccount: account, wantRole: role},
		{name: "IAM path", roleARN: "arn:aws:iam::123456789012:role/smoke/test-role", wantAccount: account, wantRole: "arn:aws:iam::123456789012:role/smoke/test-role"},
		{name: "partition", roleARN: "arn:aws-us-gov:iam::123456789012:role/test-role", wantAccount: account, wantRole: "arn:aws-us-gov:iam::123456789012:role/test-role"},
		{name: "short account", account: "1234", wantError: "12 digits"},
		{name: "non-numeric account", account: "abcdefghijkl", wantError: "12 digits"},
		{name: "missing account", roleName: "test-role", wantError: "requires --aws-account-id"},
		{name: "invalid role", account: account, roleName: "test role", wantError: "IAM role name"},
		{name: "account mismatch", account: "000000000000", roleARN: role, wantError: "account does not match"},
		{name: "role mismatch", account: account, roleName: "other-role", roleARN: role, wantError: "does not match --identity-role-name"},
		{name: "invalid ARN", roleARN: "infra-smoketest-aws", wantError: "IAM role ARN"},
		{name: "user ARN", roleARN: "arn:aws:iam::123456789012:user/test", wantError: "IAM role ARN"},
		{name: "missing role", roleARN: "arn:aws:iam::123456789012:role/", wantError: "IAM role ARN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotAccount, gotRole, err := ResolveAWSIdentity(tc.account, tc.roleName, tc.roleARN)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("want error containing %q, got %v", tc.wantError, err)
				}
				return
			}
			if err != nil || gotAccount != tc.wantAccount || gotRole != tc.wantRole {
				t.Fatalf("got account %q, role %q, error %v", gotAccount, gotRole, err)
			}
		})
	}
}

func TestIdentityPodRequiresRoleInSnapshottedAccount(t *testing.T) {
	account, role, err := ResolveAWSIdentity("123456789012", "", "")
	if err != nil {
		t.Fatal(err)
	}
	run := testRun()
	run.Status.AWSAccountID, run.Status.ExpectedRoleARN = account, role
	stage := testPodStage("identity")
	stage.CreatePod.Template.Spec.ServiceAccountName = definition.IdentitySA
	stage.CreatePod.Template.Spec.NodeSelector = nil
	for _, annotatedRole := range []string{role, "arn:aws:iam::000000000000:role/infra-smoketest-aws", ""} {
		t.Run(annotatedRole, func(t *testing.T) {
			r := fakeReconciler(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: definition.IdentitySA, Namespace: definition.Namespace, Annotations: map[string]string{"eks.amazonaws.com/role-arn": annotatedRole}}})
			// Later controller pins must not change this Run's expectation.
			r.AWSAccountID, r.ExpectedRoleARN = "000000000000", "arn:aws:iam::000000000000:role/other"
			pod, err := r.pod(context.Background(), run, stage, "probe")
			if annotatedRole != role {
				var f *failure
				if !errors.As(err, &f) || f.reason != "PreconditionFailed" {
					t.Fatalf("accepted mismatched ServiceAccount role: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			for _, e := range pod.Spec.Containers[0].Env {
				env[e.Name] = e.Value
			}
			if env["SMOKETEST_AWS_ACCOUNT_ID"] != account || env["SMOKETEST_EXPECTED_ROLE_ARN"] != role {
				t.Fatalf("probe did not inherit snapshotted identity settings: %v", env)
			}
		})
	}
}

func TestParseRoleARN(t *testing.T) {
	for _, tc := range []struct{ arn, account string }{
		{"arn:aws:iam::123456789012:role/infra-smoketest-aws", "123456789012"},
		{"arn:aws-us-gov:iam::123456789012:role/smoke/test-role", "123456789012"},
		{"", ""},
		{"infra-smoketest-aws", ""},
		{"arn:aws:iam::123456789012:user/test", ""},
		{"arn:aws:iam::1234:role/test", ""},
		{"arn:aws:sts::123456789012:assumed-role/test/session", ""},
	} {
		account, err := ParseRoleARN(tc.arn)
		if (err == nil) != (tc.account != "") || account != tc.account {
			t.Errorf("ParseRoleARN(%q) = %q, %v", tc.arn, account, err)
		}
	}
}

func identitySA(roleARN string) *corev1.ServiceAccount {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: definition.IdentitySA, Namespace: definition.Namespace}}
	if roleARN != "" {
		sa.Annotations = map[string]string{RoleAnnotation: roleARN}
	}
	return sa
}

func TestAcceptanceDerivesIdentityFromServiceAccountAnnotation(t *testing.T) {
	const role = "arn:aws:iam::123456789012:role/infra-smoketest-aws"
	const pinned = "arn:aws:iam::000000000000:role/pinned"
	for _, tc := range []struct {
		name, annotation, pinAccount, pinRole, wantAccount, wantRole string
		noServiceAccount                                             bool
	}{
		{name: "annotation", annotation: role, wantAccount: "123456789012", wantRole: role},
		{name: "path and partition", annotation: "arn:aws-us-gov:iam::123456789012:role/smoke/test", wantAccount: "123456789012", wantRole: "arn:aws-us-gov:iam::123456789012:role/smoke/test"},
		{name: "pin wins", annotation: role, pinAccount: "000000000000", pinRole: pinned, wantAccount: "000000000000", wantRole: pinned},
		{name: "no annotation"},
		{name: "malformed annotation", annotation: "arn:aws:iam::123456789012:user/test"},
		{name: "no ServiceAccount", noServiceAccount: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			test := &api.SmokeTest{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: definition.Namespace, UID: "test-uid"}, Spec: api.SmokeTestSpec{Stages: []api.Stage{testPodStage("ready")}}}
			run := &api.SmokeTestRun{ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: definition.Namespace, UID: "run-uid"}, Spec: api.SmokeTestRunSpec{TestRef: api.TestReference{Name: test.Name}}}
			objects := []client.Object{test, run}
			if !tc.noServiceAccount {
				objects = append(objects, identitySA(tc.annotation))
			}
			r := fakeReconciler(objects...)
			r.AWSAccountID, r.ExpectedRoleARN = tc.pinAccount, tc.pinRole
			// The first pass adds the finalizer; the second accepts the Run.
			for range 2 {
				if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key(run.Name)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Get(context.Background(), key(run.Name), run); err != nil {
				t.Fatal(err)
			}
			if !meta.IsStatusConditionTrue(run.Status.Conditions, "Accepted") {
				t.Fatalf("Run was not accepted: %+v", run.Status)
			}
			if run.Status.AWSAccountID != tc.wantAccount || run.Status.ExpectedRoleARN != tc.wantRole {
				t.Fatalf("snapshot account %q role %q, want %q %q", run.Status.AWSAccountID, run.Status.ExpectedRoleARN, tc.wantAccount, tc.wantRole)
			}
		})
	}
}

func TestIdentityPodRequiresExpectedRole(t *testing.T) {
	run := testRun()
	stage := testPodStage("identity")
	stage.CreatePod.Template.Spec.ServiceAccountName = definition.IdentitySA
	stage.CreatePod.Template.Spec.NodeSelector = nil
	r := fakeReconciler(identitySA("arn:aws:iam::123456789012:role/infra-smoketest-aws"))
	_, err := r.pod(context.Background(), run, stage, "probe")
	var f *failure
	if !errors.As(err, &f) || f.reason != "PreconditionFailed" || !strings.Contains(f.message, RoleAnnotation) {
		t.Fatalf("identity Pod without an expected role was accepted: %v", err)
	}
}
