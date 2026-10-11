variable "state_bucket" {
  type        = string
  description = "GCS bucket holding the infrastructure layers' state. Same bucket as backend.gcs.tfbackend."
  nullable    = false
}

# No defaults for anything naming a host: this repo is public and the domain
# stays out of it. Set them in terraform.tfvars (gitignored).

variable "domain" {
  type        = string
  description = "Base domain. The dashboard is pgcloud.homelab.internal.<domain>; databases are pg-<id>.db.homelab.internal.<domain>."
  nullable    = false
}

variable "vault_address" {
  type        = string
  description = "Vault API URL, scheme included. On the tailnet."
  nullable    = false
}

variable "argocd_server_addr" {
  type        = string
  description = "Argo CD API as host:port, no scheme."
  nullable    = false
}

variable "zitadel_url" {
  type        = string
  description = "ZITADEL base URL with scheme, e.g. https://zitadel.example.com. Also the OIDC issuer."
  nullable    = false

  validation {
    condition     = can(regex("^https://[^/:]+(:[0-9]+)?$", var.zitadel_url))
    error_message = "zitadel_url must be https://host[:port], with no path or trailing slash."
  }
}

variable "zitadel_org_id" {
  type        = string
  description = "ZITADEL organization the project and grants belong to (local.zitadel_org_id in the infrastructure repo's layer 30)."
  nullable    = false
}

variable "zitadel_user_ids" {
  type        = set(string)
  description = "ZITADEL user IDs granted pgcloud-user — who may sign in. Each gets a personal organization on first login."
  default     = []
  nullable    = false
}

variable "application_repo_url" {
  type        = string
  description = "HTTPS URL of this repo, which both Argo CD Applications render from. Public, so Argo CD needs no credentials; must be under https://github.com/igomez10/* for Terraform's Argo CD role."
  default     = "https://github.com/igomez10/kindergarten.git"
  nullable    = false
}

variable "target_revision" {
  type        = string
  description = "Branch both Applications track."
  default     = "main"
  nullable    = false
}
