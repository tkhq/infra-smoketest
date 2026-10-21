package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/tkhq/infra-smoketest/internal/definition"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// RoleAnnotation is the pod-identity webhook annotation that selects the IAM
// role for a ServiceAccount. It is the source of the expected identity.
const RoleAnnotation = "eks.amazonaws.com/role-arn"

var accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)
var roleNamePattern = regexp.MustCompile(`^[A-Za-z0-9_+=,.@-]{1,64}$`)

// ParseRoleARN returns the account of a well-formed IAM role ARN.
func ParseRoleARN(roleARN string) (string, error) {
	role, err := arn.Parse(roleARN)
	if err != nil || role.Partition == "" || role.Service != "iam" || role.Region != "" || !accountIDPattern.MatchString(role.AccountID) || !strings.HasPrefix(role.Resource, "role/") || !roleNamePattern.MatchString(role.Resource[strings.LastIndex(role.Resource, "/")+1:]) {
		return "", fmt.Errorf("%q is not an IAM role ARN", roleARN)
	}
	return role.AccountID, nil
}

// ResolveAWSIdentity resolves optional controller flags that pin the expected
// identity. Without them, each Run derives its expectation from the identity
// ServiceAccount's role annotation. A full ARN remains supported for
// deployments using an IAM path or partition other than aws.
func ResolveAWSIdentity(accountID, roleName, roleARN string) (string, string, error) {
	if accountID != "" && !accountIDPattern.MatchString(accountID) {
		return "", "", fmt.Errorf("--aws-account-id must contain exactly 12 digits")
	}
	if roleName != "" && !roleNamePattern.MatchString(roleName) {
		return "", "", fmt.Errorf("--identity-role-name must be an IAM role name; use --expected-role-arn for a role with a path")
	}
	if roleARN != "" {
		roleAccount, err := ParseRoleARN(roleARN)
		if err != nil {
			return "", "", fmt.Errorf("--expected-role-arn must be an IAM role ARN")
		}
		if accountID != "" && roleAccount != accountID {
			return "", "", fmt.Errorf("--expected-role-arn account does not match --aws-account-id")
		}
		if roleName != "" && !strings.HasSuffix(roleARN, ":role/"+roleName) {
			return "", "", fmt.Errorf("--expected-role-arn does not match --identity-role-name")
		}
		return roleAccount, roleARN, nil
	}
	if accountID == "" {
		if roleName != "" {
			return "", "", fmt.Errorf("--identity-role-name requires --aws-account-id")
		}
		return "", "", nil
	}
	if roleName == "" {
		roleName = definition.IdentitySA
	}
	return accountID, fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, roleName), nil
}

// awsIdentity returns the account and role ARN a Run should expect. Pinned
// controller flags win; otherwise the identity ServiceAccount's annotation is
// the single source, read when the Run is accepted. A missing ServiceAccount or
// an absent or malformed annotation leaves the Run without AWS settings, so
// non-identity tests still run while identity Pods fail their precondition.
func (r *Reconciler) awsIdentity(ctx context.Context) (string, string, error) {
	if r.ExpectedRoleARN != "" {
		return r.AWSAccountID, r.ExpectedRoleARN, nil
	}
	sa := &corev1.ServiceAccount{}
	if err := r.Get(ctx, key(definition.IdentitySA), sa); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", err
	}
	roleARN := sa.Annotations[RoleAnnotation]
	account, err := ParseRoleARN(roleARN)
	if err != nil {
		return "", "", nil
	}
	return account, roleARN, nil
}
