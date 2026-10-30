import type { NextConfig } from "next";
import { PHASE_DEVELOPMENT_SERVER } from "next/constants";

// Production builds are a static export embedded in the Go binary. Under
// `make dev` the gateway proxies page requests to `next dev` instead
// (FASTCLAW_DEV_WEB_URL), so the export — and its placeholder-only
// dynamic params — is skipped in the dev server.
export default function config(phase: string): NextConfig {
  return {
    ...(phase === PHASE_DEVELOPMENT_SERVER ? {} : { output: "export" as const }),
    trailingSlash: true,
    images: {
      unoptimized: true,
    },
  };
}
