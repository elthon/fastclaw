"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";

// /console/agents/<id>/ has no screen of its own; open the Agent's
// customization page.
export default function ConsoleAgentRootPage() {
  const router = useRouter();
  const agentId = useAgentIdFromURL();
  useEffect(() => {
    router.replace(`/console/agents/${encodeURIComponent(agentId)}/customize/`);
  }, [router, agentId]);
  return null;
}
