"use client";

import * as React from "react";
import { Crown, LoaderCircle, Plus, Search, UserMinus } from "lucide-react";
import { BotAvatar } from "@/components/bot-avatar";
import { useLocale } from "@/components/locale-provider";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { getAgents, getConfig, updateConfig, type AgentDetail, type TeamEntry } from "@/lib/api";

// A group needs at least two Agents to coordinate.
const MIN_MEMBERS = 2;

// TeamMembersDialog is the group's member manager: search the roster,
// remove members, and add Agents. Edits are staged and written in one
// save so the group never passes through an invalid size.
export function TeamMembersDialog({ teamId, team, onClose, onSaved }: {
  teamId: string;
  team: TeamEntry;
  onClose: () => void;
  onSaved: (team: TeamEntry, members: AgentDetail[]) => void;
}) {
  const { tr } = useLocale();
  const [agents, setAgents] = React.useState<AgentDetail[]>([]);
  const [members, setMembers] = React.useState<string[]>(team.agents);
  const [query, setQuery] = React.useState("");
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState("");

  React.useEffect(() => {
    let cancelled = false;
    getAgents().then((next) => { if (!cancelled) setAgents(next); })
      .catch((cause) => { if (!cancelled) setError(cause instanceof Error ? cause.message : String(cause)); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, []);

  const byId = React.useMemo(() => new Map(agents.map((agent) => [agent.id, agent])), [agents]);
  const needle = query.trim().toLocaleLowerCase();
  const matches = (id: string) => {
    const agent = byId.get(id);
    return `${agent?.name || ""} ${id}`.toLocaleLowerCase().includes(needle);
  };
  const lead = members.includes(team.defaultAgent || "") ? team.defaultAgent : members[0];
  const visibleMembers = members.filter(matches);
  const addable = agents.filter((agent) => !members.includes(agent.id) && matches(agent.id));
  const changed = members.length !== team.agents.length || members.some((id) => !team.agents.includes(id));

  const save = async () => {
    if (saving || !changed || members.length < MIN_MEMBERS) return;
    setSaving(true);
    setError("");
    try {
      // Re-read the group so the save keeps settings edited elsewhere.
      const config = await getConfig("user");
      const current = config.teams?.[teamId];
      if (!current) throw new Error(tr("Group chat no longer exists", "群聊已不存在"));
      const next: TeamEntry = {
        ...current,
        agents: members,
        defaultAgent: members.includes(current.defaultAgent || "") ? current.defaultAgent : members[0],
      };
      const response = await updateConfig({ teams: { [teamId]: next } }, "user");
      if (!response?.ok) throw new Error(response?.error || tr("Save failed", "保存失败"));
      onSaved(next, members.map((id) => byId.get(id)).filter((agent): agent is AgentDetail => !!agent));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setSaving(false);
    }
  };

  const row = (agent: AgentDetail | undefined, id: string, action: React.ReactNode, badge?: React.ReactNode) => (
    <div key={id} className="flex h-11 items-center gap-3 rounded-lg px-2 hover:bg-muted/60">
      <BotAvatar agentId={id} avatarUrl={agent?.avatarUrl} seed={id} size={28} />
      <span className="min-w-0 flex-1 truncate text-sm">{agent?.name || id}</span>
      {badge}
      {action}
    </div>
  );

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !saving) onClose(); }}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{tr("Manage members", "成员管理")}</DialogTitle>
          <DialogDescription>{tr("Search, add, or remove the Agents in this group. Changes apply to the next message.", "搜索、添加或移除群里的 Agent，修改从下一条消息开始生效。")}</DialogDescription>
        </DialogHeader>
        <div className="relative">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            aria-label={tr("Search members", "搜索成员")}
            placeholder={tr("Search members", "搜索成员")}
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            className="pl-9"
          />
        </div>

        <section className="space-y-1">
          <h3 className="px-2 text-xs font-medium text-muted-foreground">
            {tr("Members · {{count}}", "成员 · {{count}}", { count: members.length })}
          </h3>
          {visibleMembers.length === 0 ? (
            <p className="px-2 py-2 text-sm text-muted-foreground/75">{tr("No matching members", "没有匹配的成员")}</p>
          ) : visibleMembers.map((id) => row(
            byId.get(id),
            id,
            <Button
              size="sm"
              variant="ghost"
              className="h-8 gap-1 text-muted-foreground hover:text-destructive"
              disabled={saving || members.length <= MIN_MEMBERS}
              title={members.length <= MIN_MEMBERS ? tr("A group needs at least two members", "群聊至少需要两个成员") : undefined}
              onClick={() => setMembers((current) => current.filter((member) => member !== id))}
            >
              <UserMinus className="size-4" />
              {tr("Remove", "移除")}
            </Button>,
            id === lead ? (
              <span className="flex shrink-0 items-center gap-1 text-xs text-muted-foreground"><Crown className="size-3" />{tr("Lead", "队长")}</span>
            ) : undefined,
          ))}
        </section>

        <section className="space-y-1">
          <h3 className="px-2 text-xs font-medium text-muted-foreground">{tr("Add Agents", "添加 Agent")}</h3>
          {loading ? (
            <div className="flex justify-center py-3"><LoaderCircle className="size-4 animate-spin text-muted-foreground" /></div>
          ) : addable.length === 0 ? (
            <p className="px-2 py-2 text-sm text-muted-foreground/75">
              {needle ? tr("No matching Agents", "没有匹配的 Agent") : tr("Every Agent is already in this group", "所有 Agent 都已在群里")}
            </p>
          ) : (
            <div className="max-h-56 space-y-1 overflow-y-auto">
              {addable.map((agent) => row(
                agent,
                agent.id,
                <Button
                  size="sm"
                  variant="ghost"
                  className="h-8 gap-1"
                  disabled={saving}
                  onClick={() => setMembers((current) => [...current, agent.id])}
                >
                  <Plus className="size-4" />
                  {tr("Add", "添加")}
                </Button>,
              ))}
            </div>
          )}
        </section>

        {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
        <DialogFooter>
          <Button variant="outline" disabled={saving} onClick={onClose}>{tr("Cancel", "取消")}</Button>
          <Button disabled={loading || saving || !changed || members.length < MIN_MEMBERS} onClick={() => void save()}>
            {saving && <LoaderCircle className="size-4 animate-spin" />}
            {tr("Save", "保存")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
