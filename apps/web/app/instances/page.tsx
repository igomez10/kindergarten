"use client";

import Link from "next/link";
import { useQuery } from "@tanstack/react-query";
import { api, formatCPU, formatMemory, TRANSITIONAL } from "@/lib/api";
import { StatusBadge } from "../status";

export default function InstancesPage() {
  const q = useQuery({
    queryKey: ["instances"],
    queryFn: api.instances,
    // Poll quickly while anything is changing, slowly otherwise.
    refetchInterval: (query) => (query.state.data?.some((i) => TRANSITIONAL.has(i.status)) ? 3000 : 30000),
  });

  return (
    <section>
      <div className="page-head">
        <h1>Databases</h1>
        <Link href="/instances/new" className="button primary">Create PostgreSQL</Link>
      </div>
      {q.isLoading && <p className="muted">Loading…</p>}
      {q.error && <p className="error">{(q.error as Error).message}</p>}
      {q.data && q.data.length === 0 && (
        <div className="empty">
          <p>No databases yet.</p>
          <Link href="/instances/new" className="button primary">Create your first PostgreSQL</Link>
        </div>
      )}
      {q.data && q.data.length > 0 && (
        <table className="table">
          <thead>
            <tr><th>Name</th><th>Status</th><th>Version</th><th>Size</th><th>Endpoint</th></tr>
          </thead>
          <tbody>
            {q.data.map((i) => (
              <tr key={i.id}>
                <td><Link href={`/instances/${i.id}`}>{i.name}</Link></td>
                <td><StatusBadge status={i.status} /></td>
                <td>PostgreSQL {i.postgres_version}</td>
                <td className="muted">{i.plan} · {formatCPU(i.cpu_millis)} · {formatMemory(i.memory_mib)} · {i.storage_gib} GiB</td>
                <td className="mono">{i.endpoint ? `${i.endpoint.host}:${i.endpoint.port}` : "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
