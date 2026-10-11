# pgcloud's own Terraform root: the objects that describe how pgcloud deploys
# — its ZITADEL login, its Vault policy, its Argo CD Applications — kept in
# the repo that holds its code, like igomez10/microservices' socialapp and
# urlshortener roots.
#
# Its own state; the only thing it reads from the infrastructure repo is layer
# 30's two Vault paths (remote-state.tf).
terraform {
  required_version = ">= 1.11.0"

  required_providers {
    vault = {
      source  = "hashicorp/vault"
      version = "~> 5.0"
    }
    argocd = {
      source  = "argoproj-labs/argocd"
      version = "7.17.0"
    }
    zitadel = {
      source  = "zitadel/zitadel"
      version = "~> 3.4"
    }
  }

  # The real bucket comes from backend.gcs.tfbackend (gitignored), which
  # overrides this placeholder:
  #   terraform init -backend-config=backend.gcs.tfbackend
  backend "gcs" {
    bucket = "set-in-backend-gcs-tfbackend"
    prefix = "terraform/state/pgcloud"
  }
}
