"use client";

import { useMemo, useState } from "react";
import { useRouter } from "next/navigation";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError, formatCPU, formatMemory } from "@/lib/api";

const NAME_RE = /^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$/;

export default function NewInstancePage() {
  const router = useRouter();
  const qc = useQueryClient();
  const plans = useQuery({ queryKey: ["plans"], queryFn: api.plans });
  const [name, setName] = useState("");
  const [plan, setPlan] = useState("nano");
  const [version, setVersion] = useState("");
  // One key per form: a double click or a retried request after a network
  // error cannot create a second database.
  const idempotencyKey = useMemo(() => crypto.randomUUID(), []);

  const create = useMutation({
    mutationFn: () => api.create({ name, plan, postgres_version: version || plans.data!.default_postgres_version }, idempotencyKey),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ["instances"] });
      qc.invalidateQueries({ queryKey: ["me"] });
      router.push(`/instances/${res.instance.id}`);
    },
  });

  const nameValid = NAME_RE.test(name);
  const err = create.error as ApiError | null;

  return (
    <section className="narrow">
      <h1>Create PostgreSQL</h1>
      {plans.data && (
        <form
          className="card form"
          onSubmit={(e) => {
            e.preventDefault();
            if (nameValid) create.mutate();
          }}
        >
          <label>
            Name
            <input value={name} onChange={(e) => setName(e.target.value.toLowerCase())} placeholder="development-db" autoFocus required />
            <span className={name && !nameValid ? "hint error" : "hint"}>
              Lowercase letters, digits and hyphens; starts with a letter; up to 40 characters.
            </span>
          </label>

          <fieldset>
            <legend>Plan</legend>
            <div className="plans">
              {plans.data.plans.map((p) => (
                <label key={p.id} className={plan === p.id ? "plan selected" : "plan"}>
                  <input type="radio" name="plan" value={p.id} checked={plan === p.id} onChange={() => setPlan(p.id)} />
                  <strong>{p.display_name}</strong>
                  <span className="muted">{formatCPU(p.cpu_millis)} · {formatMemory(p.memory_mib)} · {p.storage_gib} GiB</span>
                </label>
              ))}
            </div>
          </fieldset>

          <label>
            PostgreSQL version
            <select value={version || plans.data.default_postgres_version} onChange={(e) => setVersion(e.target.value)}>
              {plans.data.postgres_versions.map((v) => <option key={v} value={v}>{v}</option>)}
            </select>
          </label>

          {err && <p className="error">{err.message}</p>}
          <div className="actions">
            <button type="submit" className="button primary" disabled={!nameValid || create.isPending}>
              {create.isPending ? "Creating…" : "Create"}
            </button>
          </div>
        </form>
      )}
      {plans.error && <p className="error">{(plans.error as Error).message}</p>}
    </section>
  );
}
