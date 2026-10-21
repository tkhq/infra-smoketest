terraform {
  required_version = ">= 1.7.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}

locals {
  # Consume the same role ARN the controller reads from the ServiceAccount.
  service_account = yamldecode(file("${path.module}/../../../../aws-pod-identity-webhook/serviceaccount.yaml"))
  role_arn        = local.service_account.metadata.annotations["eks.amazonaws.com/role-arn"]
  aws_account_id  = split(":", local.role_arn)[4]
  role_name       = trimprefix(split(":", local.role_arn)[5], "role/")
}

provider "aws" {
  region              = var.region
  allowed_account_ids = [local.aws_account_id]
}

module "identity" {
  source = "../.."

  aws_account_id    = local.aws_account_id
  role_name         = local.role_name
  oidc_provider_url = var.oidc_provider_url
  tags              = var.tags
}

output "role_arn" {
  description = "Role ARN to compare with the identity ServiceAccount annotation."
  value       = module.identity.role_arn
}
