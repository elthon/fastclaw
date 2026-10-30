"use client";

import * as React from "react";
import { Search } from "lucide-react";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { BotAvatar } from "@/components/bot-avatar";
import { TeamAvatarStack } from "@/components/team-avatar-stack";
import { useLocale } from "@/components/locale-provider";
import { cn } from "@/lib/utils";
import type { ConsumerAgentItem, ConsumerTeamItem } from "@/components/consumer-chat-sidebar";

type Entry =
  | { kind: "team"; team: ConsumerTeamItem; members: ConsumerAgentItem[]; label: string }
  | { kind: "agent"; agent: ConsumerAgentItem };

const isMac = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform);

// ChatSearchDialog is the chat sidebar's quick switcher: type to filter
// group chats and Agents, ↑/↓ + Enter or ⌘1–9 to open one.
export function ChatSearchDialog({
  open,
  onOpenChange,
  agents,
  teams,
  unreadAgentIds,
  onPickAgent,
  onPickTeam,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  agents: ConsumerAgentItem[];
  teams: ConsumerTeamItem[];
  unreadAgentIds: Set<string>;
  onPickAgent: (agent: ConsumerAgentItem) => void;
  onPickTeam: (team: ConsumerTeamItem) => void;
}) {
  const { t, tr } = useLocale();
  const [query, setQuery] = React.useState("");
  const [active, setActive] = React.useState(0);
  const listRef = React.useRef<HTMLDivElement>(null);

  const entries = React.useMemo<Entry[]>(() => {
    const q = query.trim().toLocaleLowerCase();
    const out: Entry[] = [];
    for (const team of teams) {
      const members = team.agents
        .map((id) => agents.find((agent) => agent.id === id))
        .filter((agent): agent is ConsumerAgentItem => !!agent);
      const label = members.map((member) => member.name).join("、");
      if (!q || `${team.name} ${label}`.toLocaleLowerCase().includes(q)) {
        out.push({ kind: "team", team, members, label });
      }
    }
    for (const agent of agents) {
      if (!q || `${agent.name} ${agent.description || ""}`.toLocaleLowerCase().includes(q)) {
        out.push({ kind: "agent", agent });
      }
    }
    return out;
  }, [agents, query, teams]);

  React.useEffect(() => {
    if (open) {
      setQuery("");
      setActive(0);
    }
  }, [open]);

  React.useEffect(() => {
    listRef.current
      ?.querySelector(`[data-index="${active}"]`)
      ?.scrollIntoView({ block: "nearest" });
  }, [active]);

  const pick = (entry: Entry | undefined) => {
    if (!entry) return;
    onOpenChange(false);
    if (entry.kind === "team") onPickTeam(entry.team);
    else onPickAgent(entry.agent);
  };

  const onKeyDown = (event: React.KeyboardEvent) => {
    if ((event.metaKey || event.ctrlKey) && /^[1-9]$/.test(event.key)) {
      event.preventDefault();
      pick(entries[Number(event.key) - 1]);
      return;
    }
    if (event.key === "ArrowDown") {
      event.preventDefault();
      setActive((i) => Math.min(i + 1, entries.length - 1));
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      setActive((i) => Math.max(i - 1, 0));
    } else if (event.key === "Enter" && !event.nativeEvent.isComposing) {
      event.preventDefault();
      pick(entries[active]);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        showCloseButton={false}
        className="top-[20%] translate-y-0 gap-0 overflow-hidden p-0 sm:max-w-xl"
        onKeyDown={onKeyDown}
      >
        <DialogTitle className="sr-only">{tr("Search", "搜索")}</DialogTitle>
        <div className="flex items-center gap-2.5 border-b px-4">
          <Search className="size-4 shrink-0 text-muted-foreground" />
          <input
            autoFocus
            value={query}
            onChange={(event) => {
              setQuery(event.target.value);
              setActive(0);
            }}
            placeholder={tr("Search", "搜索")}
            aria-label={t("sidebar.searchBots")}
            className="h-12 w-full bg-transparent text-[15px] outline-none placeholder:text-muted-foreground"
          />
        </div>
        <div ref={listRef} className="max-h-[min(60vh,480px)] overflow-y-auto p-2">
          {entries.length === 0 ? (
            <p className="px-3 py-8 text-center text-sm text-muted-foreground">{t("sidebar.noMatches")}</p>
          ) : (
            entries.map((entry, index) => {
              const name = entry.kind === "team" ? entry.team.name : entry.agent.name || t("sidebar.untitledBot");
              const sub = entry.kind === "team" ? entry.label : entry.agent.description;
              const unread = entry.kind === "agent" && unreadAgentIds.has(entry.agent.id);
              return (
                <button
                  key={entry.kind === "team" ? `team-${entry.team.id}` : entry.agent.id}
                  type="button"
                  data-index={index}
                  onMouseMove={() => setActive(index)}
                  onClick={() => pick(entry)}
                  className={cn(
                    "flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-left",
                    index === active && "bg-black/[0.06] dark:bg-white/[0.08]",
                  )}
                >
                  {entry.kind === "team" ? (
                    <TeamAvatarStack members={entry.members} avatarUrl={entry.team.avatarUrl} size={30} />
                  ) : (
                    <BotAvatar agentId={entry.agent.id} avatarUrl={entry.agent.avatarUrl} seed={entry.agent.id} size={30} />
                  )}
                  <span className="grid min-w-0 flex-1">
                    <span className="truncate text-sm font-medium">{name}</span>
                    {sub && <span className="truncate text-[13px] text-muted-foreground">{sub}</span>}
                  </span>
                  {unread && <span className="size-2 shrink-0 rounded-full bg-blue-600" aria-label={tr("Unread", "未读")} />}
                  {index < 9 && (
                    <kbd className="shrink-0 rounded-md border bg-muted/60 px-1.5 py-0.5 font-sans text-[11px] text-muted-foreground">
                      {isMac ? "⌘" : "Ctrl+"}{index + 1}
                    </kbd>
                  )}
                </button>
              );
            })
          )}
        </div>
      </DialogContent>
    </Dialog>
  );
}
