"use client";

import { useQuery } from "@tanstack/react-query";
import { api, formatCPU, formatMemory } from "@/lib/api";

function Meter({ label, used, limit, fmt }: { label: string; used: number; limit: number; fmt: (n: number) => string }) {
  const pct = limit ? Math.min(100, Math.round((used / limit) * 100)) : 0;
  return (
    <div className="meter">
      <div className="meter-head"><span>{label}</span><span className="muted">{fmt(used)} of {fmt(limit)}</span></div>
      <div className="meter-bar"><div style={{ width: `${pct}%` }} className={pct >= 100 ? "full" : ""} /></div>
    </div>
  );
}

export default function SettingsPage() {
  const me = useQuery({ queryKey: ["me"], queryFn: api.me, refetchInterval: 30000 });
  return (
    <section className="narrow">
      <h1>Settings</h1>
      {me.data?.organizations.map((o) => (
        <div className="card" key={o.id}>
          <h2>{o.name}</h2>
          <p className="muted">Your role: {o.role}. Usage counts databases still provisioning or being deleted.</p>
          <Meter label="Databases" used={o.usage.instances} limit={o.quota.instances} fmt={(n) => String(n)} />
          <Meter label="CPU" used={o.usage.cpu_millis} limit={o.quota.cpu_millis} fmt={formatCPU} />
          <Meter label="Memory" used={o.usage.memory_mib} limit={o.quota.memory_mib} fmt={formatMemory} />
          <Meter label="Storage" used={o.usage.storage_gib} limit={o.quota.storage_gib} fmt={(n) => `${n} GiB`} />
        </div>
      ))}
      {me.data && (
        <p className="muted">Signed in as {me.data.user.email || me.data.user.subject}.</p>
      )}
    </section>
  );
}
