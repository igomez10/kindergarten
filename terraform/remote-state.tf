# The whole coupling to the infrastructure repo: layer 30 owns the Vault KV
# mount and the kubernetes auth backend; this root hangs one policy and one
# role off the two paths it publishes.
data "terraform_remote_state" "platform_config" {
  backend = "gcs"

  config = {
    bucket = var.state_bucket
    prefix = "terraform/state/30-platform-config"
  }
}

locals {
  vault_kv_mount_path        = data.terraform_remote_state.platform_config.outputs.vault_kv_mount_path
  vault_kubernetes_auth_path = data.terraform_remote_state.platform_config.outputs.vault_kubernetes_auth_path
}
