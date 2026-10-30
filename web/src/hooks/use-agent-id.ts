"use client";

import { createContext, useContext } from "react";
import { useParams, usePathname } from "next/navigation";
import { useChatRoute } from "@/lib/chat-route";

// AgentIdContext lets a surface that isn't on an Agent URL — e.g. the
// console Agent list opening the full Agent settings dialog — tell the
// embedded Agent pages which agent they edit. Empty = read the URL.
export const AgentIdContext = createContext("");

// Static-export only generates /agents/default/..., so useParams() always
// returns "default" when an app is served for a non-default agent via the
// Go spaHandler fallback. Parse the real id from the *reactive* pathname
// instead — usePathname() updates on every client navigation, so callers
// see the new id immediately when the user switches agents (otherwise
// background fetches keep firing against the old id and the chat panel
// shows the wrong history).
export function useAgentIdFromURL(): string {
  const override = useContext(AgentIdContext);
  const chatRoute = useChatRoute();
  const pathname = usePathname();
  const params = useParams<{ id: string }>();
  if (override) return override;
  // /chat/<sessionId> doesn't name the agent; AppShell resolved it.
  if (chatRoute.status === "ready" && chatRoute.target.kind === "agent") {
    return chatRoute.target.agentId;
  }
  const m = pathname?.match(/\/agents\/([^/]+)\//);
  if (m) return m[1];
  return params?.id ?? "default";
}
