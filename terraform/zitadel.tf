# ── ZITADEL ──────────────────────────────────────────────────────────────────
# The dashboard's login: a project with role check, the OIDC application
# oauth2-proxy uses, and the kindergarten-user role. Holding the role is what "may
# use kindergarten" means; ZITADEL refuses the login for anyone without it, the
# same model as the agents control plane (infrastructure layer 30).

locals {
  host = "kindergarten.homelab.internal.${var.domain}"
}

resource "zitadel_project" "kindergarten" {
  name                   = "kindergarten"
  org_id                 = var.zitadel_org_id
  project_role_check     = true
  project_role_assertion = true
}

resource "zitadel_application_oidc" "kindergarten" {
  project_id = zitadel_project.kindergarten.id
  org_id     = var.zitadel_org_id

  name                         = "kindergarten dashboard"
  redirect_uris                = ["https://${local.host}/oauth2/callback"]
  post_logout_redirect_uris    = ["https://${local.host}/"]
  response_types               = ["OIDC_RESPONSE_TYPE_CODE"]
  grant_types                  = ["OIDC_GRANT_TYPE_AUTHORIZATION_CODE", "OIDC_GRANT_TYPE_REFRESH_TOKEN"]
  app_type                     = "OIDC_APP_TYPE_WEB"
  auth_method_type             = "OIDC_AUTH_METHOD_TYPE_BASIC"
  version                      = "OIDC_VERSION_1_0"
  dev_mode                     = false
  access_token_type            = "OIDC_TOKEN_TYPE_BEARER"
  id_token_userinfo_assertion  = true
  skip_native_app_success_page = false
}

resource "zitadel_project_role" "kindergarten_user" {
  org_id       = var.zitadel_org_id
  project_id   = zitadel_project.kindergarten.id
  role_key     = "kindergarten-user"
  display_name = "kindergarten user"
  group        = "kindergarten"
}

resource "zitadel_user_grant" "kindergarten_user" {
  for_each = var.zitadel_user_ids

  org_id     = var.zitadel_org_id
  project_id = zitadel_project.kindergarten.id
  user_id    = each.value
  role_keys  = [zitadel_project_role.kindergarten_user.role_key]
}

# Copied into Vault by hand (README.md), never via a vault_kv_secret_v2 that
# would duplicate the secret into state a second time.
output "zitadel_client_id" {
  value     = zitadel_application_oidc.kindergarten.client_id
  sensitive = true
}

output "zitadel_client_secret" {
  value     = zitadel_application_oidc.kindergarten.client_secret
  sensitive = true
}
