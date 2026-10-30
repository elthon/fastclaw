"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";
import { resolveAppLanding } from "@/lib/landing";

// /agents/ used to be both the Agent list (?manage=1) and a jump into the
// first chat. The list now lives at /console/agents/; this keeps old links
// working.
export default function AgentsIndexRedirect() {
  const router = useRouter();
  useEffect(() => {
    if (new URLSearchParams(window.location.search).get("manage") === "1") {
      router.replace("/console/agents/");
      return;
    }
    resolveAppLanding().then((url) => router.replace(url));
  }, [router]);
  return null;
}
