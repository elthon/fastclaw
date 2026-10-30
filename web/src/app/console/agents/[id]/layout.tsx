import AgentAccessGate from "@/components/agent-access-gate";

export function generateStaticParams() {
  return [{ id: "default" }];
}

// Console pages for one Agent's configuration. output:'export' bakes the
// "default" placeholder at build time; the Go server maps every real id
// onto it, and the pages read the id from the URL (useAgentIdFromURL).
export default function ConsoleAgentLayout({ children }: { children: React.ReactNode }) {
  return <AgentAccessGate>{children}</AgentAccessGate>;
}
