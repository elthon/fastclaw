"use client";

import AgentAccessGate from "@/components/agent-access-gate";

// Conversation routes are rendered by AppShell so native history navigation
// can also switch between individual Agents and groups. Other Agent pages
// still keep their access gate here.
export default function AgentLayoutClient({ children }: { children: React.ReactNode }) {
  return <AgentAccessGate>{children}</AgentAccessGate>;
}
