# ── Vault ────────────────────────────────────────────────────────────────────
# Read access to kv/kindergarten/* (oidc, cloudflare) for the Vault Secrets
# Operator, through a kubernetes auth role bound to platform-system. The values
# are written with `vault kv put` (README.md), never by Terraform.
resource "vault_policy" "kindergarten" {
  name = "kindergarten"

  policy = <<-EOT
    path "${local.vault_kv_mount_path}/data/kindergarten/*" {
      capabilities = ["read"]
    }

    path "${local.vault_kv_mount_path}/metadata/kindergarten/*" {
      capabilities = ["read", "list"]
    }
  EOT
}

resource "vault_kubernetes_auth_backend_role" "kindergarten" {
  backend   = local.vault_kubernetes_auth_path
  role_name = "kindergarten"

  bound_service_account_names      = ["default"]
  bound_service_account_namespaces = ["platform-system"]

  token_policies = [vault_policy.kindergarten.name]
  token_ttl      = 3600
}
