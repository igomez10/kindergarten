provider "vault" {
  address = var.vault_address
  # Authentication comes from VAULT_TOKEN, never from configuration.
}

# The Argo CD and ZITADEL tokens, read ephemerally so neither lands in state.
# Same path and keys the infrastructure layers and the socialapp root use.
ephemeral "vault_kv_secret_v2" "semaphore_terraform" {
  mount = local.vault_kv_mount_path
  name  = "semaphore/terraform"
}

provider "argocd" {
  server_addr = var.argocd_server_addr
  auth_token  = ephemeral.vault_kv_secret_v2.semaphore_terraform.data["argocd_auth_token"]
  grpc_web    = true
}

locals {
  # The provider wants host and port separately; var.zitadel_url is validated
  # as https://host[:port].
  zitadel_host = regex("^https://([^/:]+)", var.zitadel_url)[0]
  zitadel_port = try(regex(":([0-9]+)$", var.zitadel_url)[0], "443")
}

provider "zitadel" {
  domain       = local.zitadel_host
  port         = local.zitadel_port
  access_token = ephemeral.vault_kv_secret_v2.semaphore_terraform.data["zitadel_access_token"]
}
