// Thin client for the platform API. Same origin: the session cookie that
// oauth2-proxy issued is sent automatically, and nothing is kept in storage.

export type Plan = { id: string; display_name: string; cpu_millis: number; memory_mib: number; storage_gib: number };
export type PlansResponse = { plans: Plan[]; postgres_versions: string[]; default_postgres_version: string };

export type Instance = {
  id: string;
  organization_id: string;
  name: string;
  type: string;
  plan: string;
  postgres_version: string;
  cpu_millis: number;
  memory_mib: number;
  storage_gib: number;
  desired_state: "RUNNING" | "DELETED";
  status: string;
  status_message: string | null;
  endpoint: { host: string; port: number; address: string | null } | null;
  error: string | null;
  created_at: string;
  updated_at: string;
};

export type Operation = {
  id: string;
  instance_id: string;
  action: "CREATE" | "DELETE";
  status: string;
  error: string | null;
  created_at: string;
  completed_at: string | null;
};

export type Connection = {
  host: string;
  address: string | null;
  port: number;
  database: string;
  user: string;
  sslmode: string;
  ca_cert: string;
  ca_file_name: string;
  psql: string;
  uri: string;
};

export type Credentials = { user: string; password: string; uri: string };

export type Usage = { instances: number; cpu_millis: number; memory_mib: number; storage_gib: number };
export type Me = {
  user: { id: string; email: string; subject: string };
  organizations: { id: string; name: string; role: string; quota: Usage; usage: Usage }[];
};

export class ApiError extends Error {
  constructor(public status: number, public code: string, message: string) {
    super(message);
  }
}

async function request<T>(method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: "same-origin",
    cache: "no-store",
    // Every request carries JSON content type: the API refuses writes
    // without it, which a cross-site form post cannot set.
    headers: { "Content-Type": "application/json", ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 401) {
    // The proxy session expired; a full navigation sends the user through login.
    window.location.href = "/oauth2/sign_in?rd=" + encodeURIComponent(window.location.pathname);
    throw new ApiError(401, "unauthenticated", "Signing in again…");
  }
  const text = await res.text();
  const data = text ? JSON.parse(text) : null;
  if (!res.ok) {
    throw new ApiError(res.status, data?.error?.code ?? "error", data?.error?.message ?? res.statusText);
  }
  return data as T;
}

export const api = {
  me: () => request<Me>("GET", "/v1/me"),
  plans: () => request<PlansResponse>("GET", "/v1/plans"),
  instances: () => request<{ instances: Instance[] }>("GET", "/v1/instances").then((r) => r.instances),
  instance: (id: string) => request<Instance>("GET", `/v1/instances/${id}`),
  operations: (id: string) => request<{ operations: Operation[] }>("GET", `/v1/instances/${id}/operations`).then((r) => r.operations),
  connection: (id: string) => request<Connection>("GET", `/v1/instances/${id}/connection`),
  credentials: (id: string) => request<Credentials>("GET", `/v1/instances/${id}/credentials`),
  create: (body: { name: string; plan: string; postgres_version: string }, idempotencyKey: string) =>
    request<{ instance: { id: string } }>("POST", "/v1/instances", body, { "Idempotency-Key": idempotencyKey }),
  remove: (id: string) => request<unknown>("DELETE", `/v1/instances/${id}`),
};

// Statuses in which the platform is still working; the UI polls faster.
export const TRANSITIONAL = new Set(["PENDING", "PROVISIONING", "STARTING", "DELETING", "DEGRADED"]);

export const STATUS_LABEL: Record<string, string> = {
  PENDING: "Request accepted",
  PROVISIONING: "Provisioning",
  STARTING: "Starting",
  RUNNING: "Running",
  DEGRADED: "Degraded",
  ERROR: "Error",
  DELETING: "Deleting",
  DELETED: "Deleted",
};

export function formatMemory(mib: number) {
  return mib >= 1024 ? `${mib / 1024} GiB` : `${mib} MiB`;
}

export function formatCPU(millis: number) {
  return millis >= 1000 ? `${millis / 1000} vCPU` : `${millis}m CPU`;
}
