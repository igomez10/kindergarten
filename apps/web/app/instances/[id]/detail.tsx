"use client";

import { useState } from "react";
import { usePathname, useRouter } from "next/navigation";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError, Credentials, formatCPU, formatMemory, TRANSITIONAL } from "@/lib/api";
import { StatusBadge } from "../../status";

function Copy({ text }: { text: string }) {
  const [done, setDone] = useState(false);
  return (
    <button
      type="button"
      className="button small"
      onClick={async () => {
        await navigator.clipboard.writeText(text);
        setDone(true);
        setTimeout(() => setDone(false), 1500);
      }}
    >
      {done ? "Copied" : "Copy"}
    </button>
  );
}

function download(name: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: "application/x-pem-file" }));
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  URL.revokeObjectURL(url);
}

const STEPS = ["PENDING", "PROVISIONING", "STARTING", "RUNNING"];

export default function InstanceDetail() {
  const id = usePathname().split("/").filter(Boolean)[1] ?? "";
  const router = useRouter();
  const qc = useQueryClient();
  const [creds, setCreds] = useState<Credentials | null>(null);
  const [confirm, setConfirm] = useState("");

  const inst = useQuery({
    queryKey: ["instance", id],
    queryFn: () => api.instance(id),
    enabled: !!id && id !== "_",
    refetchInterval: (q) => (q.state.data && (TRANSITIONAL.has(q.state.data.status) || q.state.data.status === "ERROR") ? 3000 : 30000),
  });
  const ops = useQuery({
    queryKey: ["operations", id],
    queryFn: () => api.operations(id),
    enabled: !!inst.data,
    refetchInterval: inst.data && TRANSITIONAL.has(inst.data.status) ? 5000 : 60000,
  });
  const ready = inst.data?.desired_state === "RUNNING" && !!inst.data.endpoint;
  const conn = useQuery({ queryKey: ["connection", id], queryFn: () => api.connection(id), enabled: ready });

  const reveal = useMutation({ mutationFn: () => api.credentials(id), onSuccess: setCreds });
  const remove = useMutation({
    mutationFn: () => api.remove(id),
    onSuccess: () => {
      setCreds(null);
      qc.invalidateQueries({ queryKey: ["instance", id] });
      qc.invalidateQueries({ queryKey: ["instances"] });
    },
  });

  if (inst.isLoading || (!inst.data && !inst.error)) return <p className="muted">Loading…</p>;
  if (inst.error) {
    const e = inst.error as ApiError;
    return <p className="error">{e.status === 404 ? "This database does not exist or you do not have access to it." : e.message}</p>;
  }
  const i = inst.data!;
  const deleting = i.desired_state === "DELETED";
  const stepIndex = STEPS.indexOf(i.status);

  return (
    <section>
      <div className="page-head">
        <div>
          <h1>{i.name}</h1>
          <p className="muted">PostgreSQL {i.postgres_version} · {i.plan} · {formatCPU(i.cpu_millis)} · {formatMemory(i.memory_mib)} · {i.storage_gib} GiB</p>
        </div>
        <StatusBadge status={i.status} />
      </div>

      {!deleting && stepIndex >= 0 && i.status !== "RUNNING" && (
        <ol className="steps">
          {STEPS.map((s, n) => (
            <li key={s} className={n < stepIndex ? "done" : n === stepIndex ? "current" : ""}>
              {["Request accepted", "Provisioning storage", "Database starting", "Ready"][n]}
            </li>
          ))}
        </ol>
      )}
      {i.status_message && <p className="notice">{i.status_message}</p>}
      {i.error && (
        <div className="notice error-box">
          <strong>{i.status === "ERROR" ? "Needs attention" : "Temporary problem"}</strong>
          <p>{i.error}</p>
        </div>
      )}

      {ready && conn.data && (
        <div className="card">
          <h2>Connect</h2>
          <dl className="kv">
            <dt>Host</dt><dd className="mono">{conn.data.host}</dd>
            {conn.data.address && (<><dt>Address</dt><dd className="mono">{conn.data.address}</dd></>)}
            <dt>Port</dt><dd className="mono">{conn.data.port}</dd>
            <dt>Database</dt><dd className="mono">{conn.data.database}</dd>
            <dt>User</dt><dd className="mono">{conn.data.user}</dd>
            <dt>TLS</dt><dd className="mono">sslmode={conn.data.sslmode}</dd>
          </dl>
          <p className="muted">Save the server CA as <code>{conn.data.ca_file_name}</code> next to where you run psql, then:</p>
          <div className="codeline">
            <code>{conn.data.psql}</code>
            <Copy text={conn.data.psql} />
          </div>
          <div className="actions">
            <button type="button" className="button" onClick={() => download(conn.data!.ca_file_name, conn.data!.ca_cert)}>
              Download CA certificate
            </button>
            {!creds && (
              <button type="button" className="button" onClick={() => reveal.mutate()} disabled={reveal.isPending}>
                Reveal password
              </button>
            )}
          </div>
          {reveal.error && <p className="error">{(reveal.error as Error).message}</p>}
          {creds && (
            <div className="secret">
              <div className="codeline"><span className="muted">Password</span><code>{creds.password}</code><Copy text={creds.password} /></div>
              <div className="codeline"><span className="muted">URI</span><code>{creds.uri}</code><Copy text={creds.uri} /></div>
              <button type="button" className="button small" onClick={() => setCreds(null)}>Hide</button>
            </div>
          )}
        </div>
      )}

      <div className="card">
        <h2>Recent operations</h2>
        {ops.data && ops.data.length > 0 ? (
          <table className="table compact">
            <thead><tr><th>Action</th><th>Status</th><th>Started</th><th>Completed</th></tr></thead>
            <tbody>
              {ops.data.map((o) => (
                <tr key={o.id}>
                  <td>{o.action}</td>
                  <td>{o.status}{o.error ? ` — ${o.error}` : ""}</td>
                  <td>{new Date(o.created_at).toLocaleString()}</td>
                  <td>{o.completed_at ? new Date(o.completed_at).toLocaleString() : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <p className="muted">None.</p>}
      </div>

      {!deleting && (
        <div className="card danger">
          <h2>Delete database</h2>
          <p>This permanently destroys the database and all of its data. There is no undo.</p>
          <label>
            Type <strong>{i.name}</strong> to confirm
            <input value={confirm} onChange={(e) => setConfirm(e.target.value)} />
          </label>
          {remove.error && <p className="error">{(remove.error as Error).message}</p>}
          <div className="actions">
            <button type="button" className="button destructive" disabled={confirm !== i.name || remove.isPending} onClick={() => remove.mutate()}>
              {remove.isPending ? "Deleting…" : "Delete permanently"}
            </button>
          </div>
        </div>
      )}
      {i.status === "DELETED" && (
        <div className="actions"><button type="button" className="button" onClick={() => router.push("/instances")}>Back to databases</button></div>
      )}
    </section>
  );
}
