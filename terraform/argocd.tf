# ── Argo CD ──────────────────────────────────────────────────────────────────
# One project, two Applications, both rendering only this repo:
#
#   pgcloud-data  deploy/manifests  — namespace, metadata database, Vault
#                 secrets, network policy, the MetalLB pool for databases.
#                 MANUAL sync: it holds a database.
#   pgcloud-app   helm/pgcloud      — API + dashboard, controller, migration
#                 Job, RBAC. AUTOMATED: the image workflow pins a digest in
#                 values.homelab.yaml and Argo CD deploys it.
#
# Tenant namespaces and the Clusters inside them are created at runtime by the
# controller and carry no Argo tracking label, so Argo never shows or prunes
# them.

resource "argocd_project" "pgcloud" {
  metadata {
    name      = "homelab-pgcloud"
    namespace = "argocd"
  }

  spec {
    description = "pgcloud (github.com/igomez10/kindergarten): self-service PostgreSQL. pgcloud-data (namespace, metadata DB, secrets, DB address pool) and pgcloud-app (API, dashboard, controller)."

    source_repos = [var.application_repo_url]

    destination {
      server    = "https://kubernetes.default.svc"
      namespace = "platform-system"
    }

    # The databases' address pool lives with MetalLB.
    destination {
      server    = "https://kubernetes.default.svc"
      namespace = "metallb-system"
    }

    # Cluster-scoped kinds: the platform namespace and the controller's RBAC.
    # Unlike socialapp, the controller needs cluster-wide permissions — it
    # creates a namespace per organization — so its ClusterRole lives here.
    cluster_resource_whitelist {
      group = ""
      kind  = "Namespace"
    }
    cluster_resource_whitelist {
      group = "rbac.authorization.k8s.io"
      kind  = "ClusterRole"
    }
    cluster_resource_whitelist {
      group = "rbac.authorization.k8s.io"
      kind  = "ClusterRoleBinding"
    }

    namespace_resource_whitelist {
      group = "*"
      kind  = "*"
    }
  }
}

resource "argocd_application" "pgcloud_data" {
  # Removing this resource deletes the Application object only — never the
  # database it manages.
  cascade = false

  lifecycle {
    ignore_changes = [metadata[0].annotations["argocd.argoproj.io/hydrate"]]
  }

  metadata {
    name      = "pgcloud-data"
    namespace = "argocd"
  }

  spec {
    project = argocd_project.pgcloud.metadata[0].name

    source {
      repo_url        = var.application_repo_url
      target_revision = var.target_revision
      path            = "deploy/manifests"
    }

    destination {
      server    = "https://kubernetes.default.svc"
      namespace = "platform-system"
    }

    # No `automated` block: manual sync, like every database in the homelab.
    sync_policy {
      sync_options = ["CreateNamespace=false", "ServerSideApply=true", "RespectIgnoreDifferences=true"]
    }

    ignore_difference {
      group                   = "postgresql.cnpg.io"
      kind                    = "Cluster"
      managed_fields_managers = ["cloudnative-pg"]
    }
    ignore_difference {
      group                   = "secrets.hashicorp.com"
      kind                    = "VaultStaticSecret"
      managed_fields_managers = ["vault-secrets-operator"]
    }
  }
}

resource "argocd_application" "pgcloud_app" {
  cascade = false

  lifecycle {
    ignore_changes = [metadata[0].annotations["argocd.argoproj.io/hydrate"]]
  }

  metadata {
    name      = "pgcloud-app"
    namespace = "argocd"
  }

  spec {
    project = argocd_project.pgcloud.metadata[0].name

    source {
      repo_url        = var.application_repo_url
      target_revision = var.target_revision
      path            = "helm/pgcloud"

      helm {
        release_name = "pgcloud"
        value_files  = ["values.homelab.yaml"]
        # Hostnames built from var.domain, so the domain never appears in
        # this public repo.
        values = yamlencode({
          host       = local.host
          oidcIssuer = var.zitadel_url
          dbDomain   = "db.homelab.internal.${var.domain}"
        })
      }
    }

    destination {
      server    = "https://kubernetes.default.svc"
      namespace = "platform-system"
    }

    sync_policy {
      automated {
        prune     = true
        self_heal = true
      }
      sync_options = ["CreateNamespace=false", "ServerSideApply=true", "RespectIgnoreDifferences=true"]
    }

    # VSO stamps a restart annotation on the Deployments when a secret rotates.
    ignore_difference {
      group                   = "apps"
      kind                    = "Deployment"
      managed_fields_managers = ["vault-secrets-operator"]
    }
  }
}
