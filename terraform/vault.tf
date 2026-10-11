# ── Vault ────────────────────────────────────────────────────────────────────
# Read access to kv/pgcloud/* (oidc, cloudflare) for the Vault Secrets
# Operator, through a kubernetes auth role bound to platform-system. The values
# are written with `vault kv put` (README.md), never by Terraform.
resource "vault_policy" "pgcloud" {
  name = "pgcloud"

  policy = <<-EOT
    path "${local.vault_kv_mount_path}/data/pgcloud/*" {
      capabilities = ["read"]
    }

    path "${local.vault_kv_mount_path}/metadata/pgcloud/*" {
      capabilities = ["read", "list"]
    }
  EOT
}

resource "vault_kubernetes_auth_backend_role" "pgcloud" {
  backend   = local.vault_kubernetes_auth_path
  role_name = "pgcloud"

  bound_service_account_names      = ["default"]
  bound_service_account_namespaces = ["platform-system"]

  token_policies = [vault_policy.pgcloud.name]
  token_ttl      = 3600
}
