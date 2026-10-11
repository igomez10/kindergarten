import type { NextConfig } from "next";

// A static export: the Go API serves out/ itself, behind the same
// oauth2-proxy, so the dashboard and /v1 share one origin and one session.
const config: NextConfig = {
  output: "export",
  images: { unoptimized: true },
};

export default config;
