"use client";

import * as React from "react";
import { LoaderCircle } from "lucide-react";
import { usePathname, useRouter } from "next/navigation";
import { chatHref, rememberChatTarget } from "@/lib/chat-route";
import { useLocale } from "@/components/locale-provider";
import { getConfig } from "@/lib/api";

export function LegacyTeamChatRedirect() {
  const pathname = usePathname() || "";
  const router = useRouter();
  const { tr } = useLocale();

  React.useEffect(() => {
    const match = pathname.match(/^\/agents\/[^/]+\/team\/([^/]+)/);
    if (!match) return;
    const teamId = decodeURIComponent(match[1]);
    let cancelled = false;

    getConfig("user")
      .then((config) => {
        if (cancelled) return;
        const sessionId = config.teams?.[teamId]?.sessionId || `team-${teamId}`;
        rememberChatTarget(sessionId, { kind: "team", teamId });
        router.replace(chatHref(sessionId));
      })
      .catch(() => {
        if (!cancelled) {
          rememberChatTarget(`team-${teamId}`, { kind: "team", teamId });
          router.replace(chatHref(`team-${teamId}`));
        }
      });

    return () => {
      cancelled = true;
    };
  }, [pathname, router]);

  return (
    <main className="flex h-[calc(100vh-3.5rem)] items-center justify-center bg-background text-sm text-muted-foreground">
      <LoaderCircle className="mr-2 size-4 animate-spin motion-reduce:animate-none" />
      {tr("Opening group chat…", "正在进入群聊…")}
    </main>
  );
}
