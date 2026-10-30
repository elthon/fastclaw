"use client";

import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { Bot, Plus, Trash2, ImagePlus, Pencil, MoreHorizontal, UserPlus } from "lucide-react";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  adminCreateUserAgent,
  adminListAgents,
  adminListUsers,
  getStatus,
  apiFetch,
  getAgents,
  getMe,
  createAgent,
  deleteAgent,
  type AgentDetail,
} from "@/lib/api";
import { useLocale } from "@/components/locale-provider";
import { agentChatHref } from "@/lib/chat-route";
import { AgentSettingsDialog } from "@/components/agent-settings-dialog";

interface OtherAgent {
  id: string;
  name: string;
  description?: string;
  userId?: string;
  ownerUsername?: string;
  ownerDisplayName?: string;
}

interface ProvisionUser {
  id: string;
  username: string;
  email: string;
  displayName?: string;
  role: string;
  status: string;
}

const BLANK_TEMPLATE_VALUE = "__blank__";
const SELECT_USER_VALUE = "__select_user__";

// AgentAvatar tries to load /api/agents/{id}/files/avatar.png and falls
// back to the default Bot icon when the agent has no avatar yet (404).
function AgentAvatar({
  agent,
  bust,
  size = 48,
  round = false,
}: {
  agent: AgentDetail;
  bust?: number; // cache-buster ticked after upload
  size?: number;
  round?: boolean;
}) {
  const [failed, setFailed] = useState(false);
  const shape = round ? "rounded-full" : "rounded-xl";
  if (!agent.avatarUrl || failed) {
    return (
      <div
        className={`flex shrink-0 items-center justify-center ${shape} bg-primary/10 dark:bg-primary/15 border border-primary/15`}
        style={{ width: size, height: size }}
      >
        <Bot className="text-primary" style={{ width: size * 0.5, height: size * 0.5 }} />
      </div>
    );
  }
  const url = bust ? `${agent.avatarUrl}?v=${bust}` : agent.avatarUrl;
  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img
      src={url}
      alt={agent.name || agent.id}
      className={`shrink-0 ${shape} object-cover`}
      style={{ width: size, height: size }}
      onError={() => setFailed(true)}
    />
  );
}

// Agents rendered per page of the grid.
const AGENT_PAGE_SIZE = 30;

export default function AgentsPage() {
  const { tr } = useLocale();
  const [agents, setAgents] = useState<AgentDetail[]>([]);
  const [provisionTemplates, setProvisionTemplates] = useState<OtherAgent[]>([]);
  const [provisionUsers, setProvisionUsers] = useState<ProvisionUser[]>([]);
  const [isAdmin, setIsAdmin] = useState(false);
  const [loading, setLoading] = useState(true);
  // quotaLocked = true when the caller has agent_quota=0 (admin
  // provisions only). They can still browse /agents to see what's
  // been provisioned for them and jump into chat — we just hide the
  // Create button. If nothing has been provisioned yet, the empty
  // state tells them to contact their admin.
  const [quotaLocked, setQuotaLocked] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  const [provisionOpen, setProvisionOpen] = useState(false);
  // The agent whose full settings dialog is open (Edit on a card).
  const [editAgentId, setEditAgentId] = useState<string | null>(null);
  const [deleteId, setDeleteId] = useState<string | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [saving, setSaving] = useState(false);

  // Bumped after avatar upload so <img> re-fetches the new file.
  const [avatarBust, setAvatarBust] = useState<Record<string, number>>({});

  // Create dialog state
  const [newName, setNewName] = useState("");
  const [newDescription, setNewDescription] = useState("");
  const [newAvatar, setNewAvatar] = useState<File | null>(null);
  const [newAvatarPreview, setNewAvatarPreview] = useState<string | null>(null);
  const [createError, setCreateError] = useState<string | null>(null);
  const createAvatarInput = useRef<HTMLInputElement>(null);

  // Admin provisioning dialog state
  const [provisionTargetUserId, setProvisionTargetUserId] = useState("");
  const [provisionTemplateId, setProvisionTemplateId] = useState(BLANK_TEMPLATE_VALUE);
  const [provisionName, setProvisionName] = useState("");
  const [provisionDescription, setProvisionDescription] = useState("");
  const [provisionError, setProvisionError] = useState<string | null>(null);
  const [provisionLoading, setProvisionLoading] = useState(false);


  const resetCreateForm = () => {
    setNewName("");
    setNewDescription("");
    setNewAvatar(null);
    if (newAvatarPreview) URL.revokeObjectURL(newAvatarPreview);
    setNewAvatarPreview(null);
    setCreateError(null);
  };

  const resetProvisionForm = () => {
    setProvisionTargetUserId("");
    setProvisionTemplateId(BLANK_TEMPLATE_VALUE);
    setProvisionName("");
    setProvisionDescription("");
    setProvisionError(null);
  };


  const fetchAgents = async () => {
    setLoading(true);
    // /api/agents returns the caller's owned agents only. Public agents
    // owned by other users surface as separate links — they don't auto-
    // populate the dashboard list.
    // The console is the caller's own account for admins too; other
    // users' agents aren't listed here.
    const list = await getAgents().catch(() => [] as AgentDetail[]);
    setAgents(list);
    const status = await getStatus().catch(() => null);
    setIsAdmin(!!status?.isAdmin);
    setLoading(false);
  };

  // Newest first, rendered a page at a time: the next page loads when
  // the end of the grid scrolls into view.
  const ownedAgents = [...agents].sort(
    (a, b) => (Date.parse(b.createdAt || "") || 0) - (Date.parse(a.createdAt || "") || 0),
  );
  const [visibleCount, setVisibleCount] = useState(AGENT_PAGE_SIZE);
  const visibleAgents = ownedAgents.slice(0, visibleCount);
  const hasMore = visibleAgents.length < ownedAgents.length;
  const loadMoreRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = loadMoreRef.current;
    if (!el || !hasMore || typeof IntersectionObserver === "undefined") return;
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((entry) => entry.isIntersecting)) {
          setVisibleCount((count) => count + AGENT_PAGE_SIZE);
        }
      },
      { rootMargin: "200px" },
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, [hasMore, visibleCount]);

  useEffect(() => {
    fetchAgents();
  }, []);

  // Resolve quotaLocked from /api/me. agent_quota === 0 means the
  // caller can't self-create — we hide the Create button but still
  // render the list so they can see admin-provisioned agents and
  // jump into chat.
  useEffect(() => {
    let aborted = false;
    getMe()
      .then((me) => {
        if (aborted) return;
        if (me?.user?.agentQuota === 0) setQuotaLocked(true);
      })
      .catch(() => {});
    return () => {
      aborted = true;
    };
  }, []);

  async function loadProvisionData() {
    if (!isAdmin) return;
    setProvisionLoading(true);
    setProvisionError(null);
    try {
      const [usersRes, agentsRes] = await Promise.all([
        adminListUsers(),
        adminListAgents(),
      ]);
      if (usersRes?.error) throw new Error(usersRes.error);
      if (agentsRes?.error) throw new Error(agentsRes.error);
      setProvisionUsers((usersRes?.users || []) as ProvisionUser[]);
      setProvisionTemplates((agentsRes?.agents || []) as OtherAgent[]);
    } catch (err) {
      setProvisionError(err instanceof Error ? err.message : "Failed to load provisioning data");
    } finally {
      setProvisionLoading(false);
    }
  }

  function openProvisionDialog() {
    resetProvisionForm();
    setProvisionOpen(true);
    loadProvisionData();
  }

  function handleProvisionTemplateChange(value: string | null) {
    if (!value) return;
    setProvisionTemplateId(value);
    setProvisionError(null);
    if (value === BLANK_TEMPLATE_VALUE) return;
    const template = provisionTemplates.find((a) => a.id === value);
    if (!template) return;
    if (!provisionName.trim()) setProvisionName(template.name || "");
    if (!provisionDescription.trim()) setProvisionDescription(template.description || "");
  }

  async function uploadAvatar(agentID: string, file: File) {
    const fd = new FormData();
    fd.append("file", file, "avatar.png");
    await apiFetch(`/api/agents/${agentID}/files`, { method: "POST", body: fd });
    setAvatarBust((m) => ({ ...m, [agentID]: Date.now() }));
  }

  const handleCreate = async () => {
    if (!newName.trim()) return;
    setSaving(true);
    setCreateError(null);
    const resp = await createAgent({
      name: newName.trim(),
      description: newDescription.trim() || undefined,
    });
    if (resp && (resp.ok === false || resp.error)) {
      setCreateError(resp.error || tr("Failed to create agent", "创建 Agent 失败"));
      setSaving(false);
      return;
    }
    const newId: string | undefined = resp?.agent?.id;
    if (newId && newAvatar) {
      try {
        await uploadAvatar(newId, newAvatar);
      } catch {
        // non-fatal — agent is created; avatar can be retried via Edit
      }
    }
    setSaving(false);
    setCreateOpen(false);
    resetCreateForm();
    fetchAgents();
  };

  const handleProvision = async () => {
    if (!provisionTargetUserId || !provisionName.trim()) return;
    setSaving(true);
    setProvisionError(null);
    const payload: { name: string; description?: string; forkFrom?: string } = {
      name: provisionName.trim(),
    };
    const desc = provisionDescription.trim();
    if (desc) payload.description = desc;
    if (provisionTemplateId !== BLANK_TEMPLATE_VALUE) payload.forkFrom = provisionTemplateId;

    try {
      const resp = await adminCreateUserAgent(provisionTargetUserId, payload);
      if (resp && (resp.ok === false || resp.error)) {
        setProvisionError(resp.error || "Failed to provision agent");
        return;
      }
      setProvisionOpen(false);
      resetProvisionForm();
      fetchAgents();
    } catch (err) {
      setProvisionError(err instanceof Error ? err.message : "Failed to provision agent");
    } finally {
      setSaving(false);
    }
  };


  const handleDelete = async () => {
    if (!deleteId) return;
    setDeleting(true);
    setDeleteError(null);
    try {
      await deleteAgent(deleteId);
      setDeleteId(null);
      fetchAgents();
    } catch (err) {
      setDeleteError(err instanceof Error ? err.message : tr("Failed to delete agent", "删除 Agent 失败"));
    } finally {
      setDeleting(false);
    }
  };

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">{tr("Agents", "Agent")}</h2>
          <p className="text-sm text-muted-foreground mt-1">
            {tr("Manage your AI agents and their configurations", "管理你的 AI Agent 及其配置")}
          </p>
        </div>
        <div className="flex items-center gap-2">
          {isAdmin && (
            <Button variant="outline" onClick={openProvisionDialog}>
              <UserPlus className="h-4 w-4 mr-2" />
              Provision Agent
            </Button>
          )}
          {!quotaLocked && (
            <Button onClick={() => setCreateOpen(true)}>
              <Plus className="h-4 w-4 mr-2" />
              {tr("New Agent", "新建 Agent")}
            </Button>
          )}
        </div>
      </div>

      {loading ? (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {[1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-48" />
          ))}
        </div>
      ) : ownedAgents.length === 0 ? (
        <div className="rounded-lg border border-border bg-card">
          <div className="flex flex-col items-center justify-center py-16 text-center">
            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-primary/10 mb-4">
              <Bot className="h-7 w-7 text-primary" />
            </div>
            <p className="text-sm text-muted-foreground">
              {quotaLocked
                ? tr("No agent has been provisioned for your account yet — contact your admin.", "你的账户尚未分配 Agent，请联系管理员。")
                : tr("No agents configured yet", "还没有配置 Agent")}
            </p>
            {!quotaLocked && (
              <Button
                onClick={() => setCreateOpen(true)}
                variant="outline"
                className="mt-4"
              >
                {tr("Create your first agent", "创建第一个 Agent")}
              </Button>
            )}
          </div>
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {visibleAgents.map((agent) => (
            <div
              key={agent.id}
              className="group relative flex h-full cursor-pointer flex-col gap-3 rounded-2xl border border-border bg-card p-4 transition-colors hover:bg-muted/40"
              onClick={() => (window.location.href = agentChatHref(agent.id))}
            >
              {/* Header: avatar, then name over the agent id. The right
                  padding keeps the name clear of the corner tag / menu. */}
              <div className={`flex min-w-0 items-center gap-3 ${agent.isPublic ? "pr-14" : "pr-8"}`}>
                <AgentAvatar agent={agent} bust={avatarBust[agent.id]} size={44} round />
                <div className="min-w-0">
                  <p className="truncate text-base font-semibold">{agent.name || agent.id}</p>
                  <p className="mt-0.5 truncate font-mono text-xs text-muted-foreground">{agent.id}</p>
                </div>
              </div>
              {/* Public is a corner tag; it gives way to the ⋯ menu on
                  hover, like a session row's run status. */}
              {agent.isPublic && (
                <span className={`pointer-events-none absolute right-3 top-4 inline-flex h-6 items-center rounded-md bg-emerald-500/10 px-2 text-xs text-emerald-700 dark:text-emerald-400 ${
                  quotaLocked ? "" : "transition-opacity group-hover:opacity-0 group-focus-within:opacity-0"
                }`}>
                  {tr("Public", "公开")}
                </span>
              )}
              {/* Always two lines tall, so cards in a row line up. */}
              <p className={`line-clamp-2 min-h-10 text-sm leading-5 ${agent.description ? "text-muted-foreground" : "text-muted-foreground/50"}`}>
                {agent.description || tr("No description", "暂无描述")}
              </p>
              {/* Tags only when there's something to say: private is the
                  default and gets no chip. */}
              {agent.model && (
              <div className="mt-auto flex min-w-0 flex-wrap gap-1.5">
                {agent.model && (
                  <span className="inline-flex h-6 min-w-0 max-w-full items-center truncate rounded-md bg-muted px-2 text-xs text-muted-foreground" title={agent.model}>
                    {agent.model}
                  </span>
                )}
              </div>
              )}
              {/* quotaLocked users (agent_quota=0) are admin-provisioned —
                  they can browse and chat but can't mutate the agent
                  record, so the Edit / Remove menu is hidden entirely. */}
              {!quotaLocked && (
                <DropdownMenu>
                  <DropdownMenuTrigger
                    render={
                      <button
                        type="button"
                        onClick={(e) => e.stopPropagation()}
                        className="absolute right-3 top-3 flex size-8 items-center justify-center rounded-lg text-muted-foreground opacity-0 transition-opacity hover:bg-background hover:text-foreground focus-visible:opacity-100 group-hover:opacity-100 aria-expanded:opacity-100"
                        aria-label={tr("Actions for {{name}}", "{{name}} 的操作", { name: agent.name || agent.id })}
                      >
                        <MoreHorizontal className="size-4" />
                      </button>
                    }
                  />
                  <DropdownMenuContent align="end" className="w-36" onClick={(e) => e.stopPropagation()}>
                    <DropdownMenuItem onClick={() => setEditAgentId(agent.id)}>
                      <Pencil className="size-4" />
                      {tr("Edit", "编辑")}
                    </DropdownMenuItem>
                    <DropdownMenuItem
                      onClick={() => setDeleteId(agent.id)}
                      className="text-destructive focus:text-destructive"
                    >
                      <Trash2 className="size-4 text-destructive" />
                      {tr("Remove", "移除")}
                    </DropdownMenuItem>
                  </DropdownMenuContent>
                </DropdownMenu>
              )}
            </div>
          ))}
        </div>
      )}
      {hasMore && (
        <div ref={loadMoreRef} className="flex justify-center py-4">
          <Button variant="ghost" size="sm" onClick={() => setVisibleCount((count) => count + AGENT_PAGE_SIZE)}>
            {tr("Load more", "加载更多")}
            <span className="ml-1 tabular-nums text-muted-foreground">{ownedAgents.length - visibleAgents.length}</span>
          </Button>
        </div>
      )}

      {/* Create Dialog */}
      <Dialog
        open={createOpen}
        onOpenChange={(v) => {
          setCreateOpen(v);
          if (!v) resetCreateForm();
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{tr("Create New Agent", "新建 Agent")}</DialogTitle>
            <DialogDescription>
              {tr("The system generates a globally unique ID (for example", "系统会生成全局唯一 ID（例如")} {" "}
              <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">agt_a1b2c3…</code>
              {tr("); everything below is for display.", "），以下内容用于展示。")}
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="flex items-start gap-4">
              <button
                type="button"
                onClick={() => createAvatarInput.current?.click()}
                className="group relative flex size-20 shrink-0 items-center justify-center overflow-hidden rounded-xl border border-dashed bg-muted/40 transition hover:bg-muted"
                aria-label={tr("Upload avatar", "上传头像")}
              >
                {newAvatarPreview ? (
                  // eslint-disable-next-line @next/next/no-img-element
                  <img src={newAvatarPreview} alt={tr("Agent avatar", "Agent 头像")} className="size-full object-cover" />
                ) : (
                  <ImagePlus className="size-6 text-muted-foreground" />
                )}
                <input
                  ref={createAvatarInput}
                  type="file"
                  accept="image/*"
                  className="hidden"
                  onChange={(e) => {
                    const f = e.target.files?.[0] ?? null;
                    setNewAvatar(f);
                    if (newAvatarPreview) URL.revokeObjectURL(newAvatarPreview);
                    setNewAvatarPreview(f ? URL.createObjectURL(f) : null);
                  }}
                />
              </button>
              <div className="flex-1 space-y-2">
                <Label htmlFor="agent-name">{tr("Name", "名称")}</Label>
                <Input
                  id="agent-name"
                  value={newName}
                  onChange={(e) => {
                    setNewName(e.target.value);
                    setCreateError(null);
                  }}
                  placeholder={tr("My Helper", "我的助手")}
                  autoFocus
                />
              </div>
            </div>
            <div className="space-y-2">
              <Label htmlFor="agent-desc">{tr("Description (optional)", "说明（可选）")}</Label>
              <Textarea
                id="agent-desc"
                value={newDescription}
                onChange={(e) => setNewDescription(e.target.value)}
                placeholder={tr("What is this agent for? Shown in the agent list and on its profile.", "这个 Agent 有什么用途？内容会显示在 Agent 列表和资料页中。")}
                rows={3}
              />
            </div>
            {createError && (
              <p className="text-sm text-destructive">{createError}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>
              {tr("Cancel", "取消")}
            </Button>
            <Button onClick={handleCreate} disabled={!newName.trim() || saving}>
              {saving ? tr("Creating…", "正在创建…") : tr("Create Agent", "创建 Agent")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Provision Dialog */}
      <Dialog
        open={provisionOpen}
        onOpenChange={(v) => {
          setProvisionOpen(v);
          if (!v) resetProvisionForm();
        }}
      >
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>Provision Agent</DialogTitle>
            <DialogDescription>
              Create an agent for a user, optionally starting from an existing template.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <Label>Create for</Label>
              <Select
                value={provisionTargetUserId || SELECT_USER_VALUE}
                onValueChange={(v) => {
                  if (!v) return;
                  setProvisionTargetUserId(v === SELECT_USER_VALUE ? "" : v);
                  setProvisionError(null);
                }}
              >
                <SelectTrigger className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={SELECT_USER_VALUE} disabled>
                    Select a user
                  </SelectItem>
                  {provisionUsers.map((user) => (
                    <SelectItem key={user.id} value={user.id}>
                      <div className="flex min-w-0 flex-col">
                        <span className="truncate">
                          {user.displayName || user.username}
                        </span>
                        <span className="truncate text-xs text-muted-foreground">
                          {user.email || user.id}
                        </span>
                      </div>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-2">
              <Label>Start from</Label>
              <Select
                value={provisionTemplateId}
                onValueChange={handleProvisionTemplateChange}
              >
                <SelectTrigger className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={BLANK_TEMPLATE_VALUE}>Blank agent</SelectItem>
                  {provisionTemplates.map((agent) => (
                    <SelectItem key={agent.id} value={agent.id}>
                      <div className="flex min-w-0 flex-col">
                        <span className="truncate">{agent.name || agent.id}</span>
                        <span className="truncate text-xs text-muted-foreground">
                          Owner: {agent.ownerDisplayName || agent.ownerUsername || agent.userId}
                        </span>
                      </div>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-2">
              <Label htmlFor="provision-agent-name">Name</Label>
              <Input
                id="provision-agent-name"
                value={provisionName}
                onChange={(e) => {
                  setProvisionName(e.target.value);
                  setProvisionError(null);
                }}
                placeholder="Tenant support agent"
                autoFocus
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="provision-agent-desc">Description (optional)</Label>
              <Textarea
                id="provision-agent-desc"
                value={provisionDescription}
                onChange={(e) => setProvisionDescription(e.target.value)}
                placeholder="Shown in the agent list and on its profile."
                rows={3}
              />
            </div>
            {provisionError && (
              <p className="text-sm text-destructive">{provisionError}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setProvisionOpen(false)}>
              Cancel
            </Button>
            <Button
              onClick={handleProvision}
              disabled={
                !provisionTargetUserId ||
                !provisionName.trim() ||
                saving ||
                provisionLoading
              }
            >
              {saving ? "Provisioning..." : "Provision Agent"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Edit Dialog */}
      {/* Edit opens the same full Agent settings dialog as the chat
          (profile, customize, models, skills, channels, …). Closing it
          refreshes the list and re-fetches the avatar. */}
      <AgentSettingsDialog
        open={editAgentId !== null}
        agentId={editAgentId || ""}
        onOpenChange={(open) => {
          if (open) return;
          if (editAgentId) {
            setAvatarBust((prev) => ({ ...prev, [editAgentId]: Date.now() }));
          }
          setEditAgentId(null);
          fetchAgents();
        }}
      />

      {/* Delete Confirmation */}
      <AlertDialog
        open={!!deleteId}
        onOpenChange={(open) => {
          if (!open && !deleting) {
            setDeleteId(null);
            setDeleteError(null);
          }
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{tr("Delete Agent", "删除 Agent")}</AlertDialogTitle>
            <AlertDialogDescription>
              {tr(
                "Are you sure you want to delete {{agent}}? This action cannot be undone.",
                "确定要删除 {{agent}} 吗？此操作无法撤销。",
                { agent: deleteId || "" },
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {deleteError && (
            <p className="text-sm text-destructive">{deleteError}</p>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>{tr("Cancel", "取消")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault();
                handleDelete();
              }}
              disabled={deleting}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {deleting ? tr("Deleting…", "正在删除…") : tr("Delete", "删除")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

    </div>
  );
}
