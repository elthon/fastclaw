"use client";

import { SkillsManager } from "@/components/skills-manager";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";

export default function AgentSkillsPage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  return <SkillsManager target={{ kind: "agent", agentId, agentName }} />;
}
