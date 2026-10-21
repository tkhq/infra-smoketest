# Smoke-test IAM role

This module creates the IAM role used by the `aws-pod-identity-webhook` smoke
test in an existing AWS account. It uses an existing IAM OIDC provider and
trusts exactly `system:serviceaccount:infra-smoketest:infra-smoketest-aws`, with
the `sts.amazonaws.com` audience and `sts:AssumeRoleWithWebIdentity` action.
This follows the [AWS IRSA trust configuration](https://docs.aws.amazon.com/eks/latest/userguide/associate-service-account-role.html).

The Kubernetes ServiceAccount is installed by
[the Kustomize configuration](../../aws-pod-identity-webhook), which is included
in `config/default`. Terraform manages the AWS role. No IAM user, access keys,
AWS account, or cluster OIDC provider is created. No permissions policy is
attached: the probe calls
[`GetCallerIdentity`, which requires no permissions](https://docs.aws.amazon.com/STS/latest/APIReference/API_GetCallerIdentity.html).

## Use this repository's ServiceAccount annotation

The runnable [examples/from-repo](examples/from-repo) configuration reads the
`eks.amazonaws.com/role-arn` annotation directly from
[the identity ServiceAccount](../../aws-pod-identity-webhook/serviceaccount.yaml),
the same annotation the controller reads. Configure the ARN there once. The
example checks that AWS credentials belong to that account and creates the
role at path `/`, matching the annotation.

Supply the HTTPS issuer URL of the cluster's existing IAM OIDC provider. For
self-managed clusters this can be the S3 issuer URL from the cluster's OIDC
Terraform outputs; it does not need to be an EKS issuer. The provider must
already include `sts.amazonaws.com` in its client ID list.

From the repository root, with AWS credentials configured for the target
account:

```sh
nix develop path:.
export TF_VAR_oidc_provider_url='https://YOUR-CLUSTER-OIDC-ISSUER'
terraform -chdir=config/terraform/aws/examples/from-repo init
terraform -chdir=config/terraform/aws/examples/from-repo plan -out=identity.tfplan
terraform -chdir=config/terraform/aws/examples/from-repo apply identity.tfplan
terraform -chdir=config/terraform/aws/examples/from-repo output -raw role_arn
```

The example defaults to `us-east-1`; set `TF_VAR_region` to change it. It uses
local Terraform state unless you add your backend configuration. Keep the state
for subsequent role updates or deletion; state, local variable files, and saved
plans are ignored by Git. The example's provider lock file is tracked.

After the role exists, apply `config/default`, then create a fresh identity
`SmokeTestRun`. The Terraform output should match the ServiceAccount role
annotation.

## Use from an existing Terraform configuration

The module inherits the caller's AWS provider. Pass the account and role name
from your configuration, along with the existing OIDC provider URL:

```hcl
module "smoke_test_identity" {
  source = "PATH/TO/infra-smoketest/config/terraform/aws"

  aws_account_id    = local.aws_account_id
  role_name         = local.smoke_test_role_name
  oidc_provider_url = module.cluster_oidc.issuer
  tags              = { Environment = "dev" }
}
```

| Input | Required | Purpose |
| --- | --- | --- |
| `aws_account_id` | Yes | Existing AWS account; checked against provider credentials. |
| `role_name` | Yes | Role name matching the ServiceAccount annotation, without an IAM path. |
| `oidc_provider_url` | Yes | HTTPS URL of the existing IAM OIDC provider in that account. |
| `tags` | No | Additional role tags; defaults to `{}`. |

Outputs are `role_arn`, `role_name`, and `aws_account_id`. The namespace and
ServiceAccount name match the controller's supported identity probe and are
fixed in this module.

## Validate without AWS access

```sh
nix develop path:. --command make terraform-check
```

This checks formatting, validates the module and example, and runs Terraform
tests with a mocked AWS provider. The tests check the exact trust policy and
reject the wrong AWS account, a provider in another account, and a missing STS
audience. No AWS credentials or live infrastructure are used by these checks.
Initialization downloads the locked AWS provider. `make check` includes them,
so GitHub Actions checks the Terraform alongside the controller.
