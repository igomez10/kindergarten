#!/usr/bin/env bash
# Local development against the homelab: metadata DB in Docker (make test-db),
# API in dev auth mode, controller reaching databases through port-forwards.
#
#   scripts/dev-env.sh api|controller [extra env assignments...]
#
# Instances go into per-organization tenant namespaces, exposed as
# <name>.$DB_DOMAIN when DB_DOMAIN is set (scripts/dev.env, gitignored, e.g.
# DB_DOMAIN=db.homelab.internal.example.com and CLOUDFLARE_ZONE=example.com).
# The Cloudflare token is read from cert-manager's DNS-01 Secret at launch and
# only ever lives in the controller's environment.
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f scripts/dev.env ] && set -a && . scripts/dev.env && set +a
export DATABASE_URL="${DATABASE_URL:-postgres://postgres:postgres@127.0.0.1:55432/kindergarten?sslmode=disable}"
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/homelab}"
what=$1; shift
case "$what" in
  api)
    exec env AUTH_MODE=dev LISTEN_ADDR=127.0.0.1:18080 MIGRATE_ON_START=true WEB_DIR=apps/web/out HEALTH_ADDR=127.0.0.1:18082 DEV_USER="${DEV_USER:-alice}" "$@" ./bin/api ;;
  controller)
    if [ -n "${CLOUDFLARE_ZONE:-}" ] && [ -z "${CLOUDFLARE_API_TOKEN:-}" ]; then
      CLOUDFLARE_API_TOKEN=$(kubectl get secret -n cert-manager cloudflare-api-token -o jsonpath='{.data.api-token}' | base64 -d)
      export CLOUDFLARE_API_TOKEN
    fi
    exec env CONN_CHECK=portforward METRICS_ADDR=127.0.0.1:18081 "$@" ./bin/controller ;;
  *) echo "usage: $0 api|controller" >&2; exit 2 ;;
esac
