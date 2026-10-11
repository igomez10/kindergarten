# Homelab infrastructure audit (Phase 1)

Recorded 2026-10-10 against the homelab cluster (`KUBECONFIG=~/.kube/homelab`).
The cluster itself is defined in `igomez10/microservices-infrastructure`; this
platform only consumes it.

## Cluster

| Item | Value |
|---|---|
| Kubernetes | v1.36.4 (kubeadm) |
| Nodes | k8s-04 (control plane, 8 CPU / 16 GiB), k8s-02 (worker, 4 CPU / 16 GiB, Kata/gVisor runtimes), k8s-03 (worker, 8 CPU / 16 GiB) |
| Requested today | CPU 47–70 %, memory 38–51 % per node |
| CNI | Cilium. NetworkPolicy is enforced; the agents app has verified policies |
| LoadBalancer | MetalLB L2, pool `lan-pool` = `10.0.0.232-10.0.0.250`. Only **.232–.235 are free**. The router keeps `.220–.231` out of DHCP as headroom. k8s-04 advertises the pool to the tailnet as a subnet route |
| Ingress | Istio gateway. Each UI gets its own MetalLB IP, a cert-manager Let's Encrypt certificate (Cloudflare DNS-01) and a Cloudflare A record under `*.homelab.internal.<domain>` |
| Identity | ZITADEL (URL and organization ID passed to Terraform as `zitadel_url` and `zitadel_org_id`) |
| Secrets | Vault + Vault Secrets Operator (`kv` mount, `kubernetes` auth) |
| GitOps | Argo CD. The Terraform account may manage projects whose repos are under `https://github.com/igomez10/*` |
| Pod Security | App namespaces are labelled `enforce: baseline`, `audit/warn: restricted` |

## Storage

| StorageClass | Provisioner | Expansion | Binding | Notes |
|---|---|---|---|---|
| `longhorn` | driver.longhorn.io | yes | WaitForFirstConsumer | **Used for managed databases.** 1 replica, `dataLocality: best-effort`, reclaim `Delete` |
| `longhorn-static` | driver.longhorn.io | yes | Immediate | |
| `local-path` (default) | rancher.io/local-path | no | WaitForFirstConsumer | Ties a volume to one node; not used |

Longhorn disk available: k8s-02 93 GiB, k8s-03 73 GiB, k8s-04 77 GiB (231 GiB
each, already over-scheduled). With one replica, losing a node's disk loses the
database on it. That is why backups (Phase 7) are required before inviting
users.

## CloudNativePG

- Operator **1.30.0** in `cnpg-system`, installed by infrastructure layer 20
  (`helm_release.cloudnative_pg`, chart 0.29.0). It is not managed from here.
- The CRD `clusters.postgresql.cnpg.io` is present. 5 clusters already run on
  it: airflow, dify, semaphore, socialapp, urlshortener.
- No `ClusterImageCatalog`. The operator default operand is
  `ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie`.
- Pinned operand images the platform offers (tags verified to exist):
  `17` → `ghcr.io/cloudnative-pg/postgresql:17.10-system-trixie`,
  `16` → `16.13-system-trixie`, `18` → `18.4-system-trixie`.
- No Barman Cloud Plugin installed yet.

## Phase 1 results (`deploy/examples/`)

| Test | Result |
|---|---|
| Create `Cluster` (1 instance, 5Gi longhorn, 250m/512Mi) | Ready in ~60 s. Created pod `test-postgres-1`, Services `-rw`, `-ro`, `-r`, PVC `test-postgres-1`, and Secrets `-app` (basic-auth: `username`, `password`, `host`, `port`, `dbname`, `uri`, `jdbc-uri`, `fqdn-uri`, `pgpass`, …), `-ca`, `-server`, `-replication` |
| Connect via `kubectl port-forward svc/test-postgres-rw` with `sslmode=require` as `app` | OK |
| Insert a row, delete the pod, reconnect | Row still present |
| Delete the `Cluster` | **Everything is removed.** The PVC carries an `ownerReference` to the Cluster (`controller: true`), so garbage collection deletes it. The PV is `Released`, then deleted by the `Delete` reclaim policy. The Longhorn volume is gone about 2 minutes later |

Consequences for the platform:

- **"Delete data" policy** = delete the `Cluster` and wait until the PVCs and
  the Longhorn volume are gone. No extra PVC cleanup is needed, but the
  reconciler must still *verify* that the PVCs are gone before it reports
  `DELETED`.
- **"Retain data" policy** would need the PVC's ownerReference removed (or a
  `Retain` StorageClass) *before* the Cluster is deleted. Not implemented in v1.
- `platform-test` stays as the single, administrator-made tenant namespace
  for Phases 2–4.
