# kindergarten — Terraform

kindergarten's own Terraform root, in the repo that holds its code (same shape as
`socialapp/terraform` in igomez10/microservices).

| File | Resources |
|---|---|
| `zitadel.tf` | Project `kindergarten` (role check on), OIDC app for oauth2-proxy, role `kindergarten-user`, one grant per `var.zitadel_user_ids` |
| `vault.tf` | Policy `kindergarten` (read `kv/kindergarten/*`), kubernetes auth role `kindergarten` bound to `platform-system` |
| `argocd.tf` | Project `homelab-kindergarten`; Applications `kindergarten-data` (manual) and `kindergarten-app` (automated) |
| `remote-state.tf` | The one read of the infrastructure repo: layer 30's Vault paths |

State: `gs://<state bucket>/terraform/state/kindergarten`. The bucket is set in
`backend.gcs.tfbackend` (gitignored; copy `backend.gcs.tfbackend.example`).

## Credentials

- `VAULT_TOKEN` with read on `kv/semaphore/terraform` (Argo CD and ZITADEL
  tokens are read from there, ephemerally).
- Google application-default credentials (state, and the layer 30 read).
- `terraform.tfvars` (gitignored; copy `terraform.tfvars.example`). Every
  environment-specific value lives there, so nothing in this public repo names
  a host, an organization or a bucket.

## First deployment

```bash
terraform init -backend-config=backend.gcs.tfbackend
terraform plan && terraform apply

# oauth2-proxy's client and cookie secret
vault kv put kv/kindergarten/oidc \
  client_id="$(terraform output -raw zitadel_client_id)" \
  client_secret="$(terraform output -raw zitadel_client_secret)" \
  cookie_secret="$(python3 -c 'import secrets,base64;print(base64.urlsafe_b64encode(secrets.token_bytes(32)).decode())')"

# The controller's Cloudflare token: Zone.DNS edit + Zone read on the zone only.
read -rs 'TOKEN?Cloudflare token: ' && vault kv put kv/kindergarten/cloudflare api_token="$TOKEN" zone="<domain>"; unset TOKEN
```

Then sync `kindergarten-data` by hand in Argo CD (namespace, metadata DB, secrets);
`kindergarten-app` syncs itself.

Letting someone in: add their ZITADEL user ID to `zitadel_user_ids` and apply.
