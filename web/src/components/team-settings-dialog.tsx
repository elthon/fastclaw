"use client";

import * as React from "react";
import { LoaderCircle, Upload, X } from "lucide-react";
import { TeamAvatarStack } from "@/components/team-avatar-stack";
import { useLocale } from "@/components/locale-provider";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { getAgents, getConfig, updateConfig, type AgentDetail, type TeamEntry } from "@/lib/api";

export function TeamSettingsDialog({ teamId, team, onClose, onSaved }: {
  teamId: string; team: TeamEntry; onClose: () => void;
  onSaved: (team: TeamEntry, members: AgentDetail[]) => void;
}) {
  const { tr } = useLocale();
  const [name, setName] = React.useState(team.name || "");
  const [description, setDescription] = React.useState(team.description || "");
  const [humanName, setHumanName] = React.useState(team.humanName || "");
  const [avatarUrl, setAvatarUrl] = React.useState(team.avatarUrl || "");
  const fileRef = React.useRef<HTMLInputElement>(null);
  // Membership is edited in TeamMembersDialog; here the roster only feeds
  // the lead picker.
  const selected = team.agents;
  const [lead, setLead] = React.useState(team.defaultAgent || team.agents[0]);
  const [agents, setAgents] = React.useState<AgentDetail[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState("");
  React.useEffect(() => {
    let cancelled = false;
    getAgents().then((next) => { if (!cancelled) setAgents(next); })
      .catch((e) => { if (!cancelled) setError(String(e)); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, []);
  const save = async () => {
    if (saving || !name.trim()) return;
    setSaving(true); setError("");
    try {
      const config = await getConfig("user");
      const current = config.teams?.[teamId];
      if (!current) throw new Error(tr("Group no longer exists", "群聊已不存在"));
      const next: TeamEntry = { ...current, name: name.trim(), description: description.trim(), humanName: humanName.trim(), avatarUrl,
        defaultAgent: current.agents.includes(lead) ? lead : current.agents[0], groupBehavior: "coordinated" };
      const result = await updateConfig({ teams: { [teamId]: next } }, "user");
      if (!result?.ok) throw new Error(result?.error || tr("Save failed", "保存失败"));
      onSaved(next, next.agents.flatMap((id) => agents.filter((agent) => agent.id === id)));
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    finally { setSaving(false); }
  };
  const pickAvatar = async (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (!file) return;
    setError("");
    if (!file.type.startsWith("image/")) {
      setError(tr("Choose an image file", "请选择图片文件"));
      return;
    }
    try {
      setAvatarUrl(await downsizeAvatar(file));
    } catch {
      setError(tr("Could not read this image", "无法读取这张图片"));
    }
  };
  const members = selected.map((id) => ({ id, avatarUrl: agents.find((agent) => agent.id === id)?.avatarUrl }));
  return <Dialog open onOpenChange={(open) => { if (!open && !saving) onClose(); }}>
    <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-lg">
      <DialogHeader><DialogTitle>{tr("Group settings", "群聊设置")}</DialogTitle>
        <DialogDescription>{tr("Set the group profile and lead. Changes apply to the next message.", "设置群资料和队长，修改从下一条消息开始生效。")}</DialogDescription></DialogHeader>
      <div className="flex items-center gap-4">
        <div className="group relative">
          <TeamAvatarStack members={members} avatarUrl={avatarUrl} size={56} />
          {avatarUrl && (
            <button type="button" onClick={() => setAvatarUrl("")}
              aria-label={tr("Remove avatar", "移除头像")} title={tr("Remove avatar", "移除头像")}
              className="absolute -right-1 -top-1 hidden size-5 items-center justify-center rounded-full border border-border bg-background text-muted-foreground shadow-sm transition hover:border-destructive hover:text-destructive group-hover:flex">
              <X className="size-3" />
            </button>
          )}
        </div>
        <div className="space-y-1">
          <Button type="button" variant="outline" size="sm" onClick={() => fileRef.current?.click()}>
            <Upload className="size-4" />{tr("Upload avatar", "上传头像")}
          </Button>
          <p className="text-xs text-muted-foreground">{tr("Without one, the group shows its members' avatars.", "不设置时显示成员头像。")}</p>
        </div>
        <input ref={fileRef} type="file" accept="image/*" className="hidden" onChange={(e) => void pickAvatar(e)} />
      </div>
      <label className="space-y-1 text-sm">{tr("Name", "群名称")}<Input value={name} onChange={(e) => setName(e.target.value)} /></label>
      <label className="space-y-1 text-sm">{tr("Description", "群简介")}<Textarea value={description} onChange={(e) => setDescription(e.target.value)} placeholder={tr("What this group works on", "这个群一起做什么")} rows={3} className="resize-none" /></label>
      <label className="space-y-1 text-sm">{tr("Your name in this group", "群成员对你的称呼")}<Input value={humanName} onChange={(e) => setHumanName(e.target.value)} /></label>
      <label className="space-y-1 text-sm">{tr("Lead member", "队长")}
        <select className="h-9 w-full rounded-md border bg-background px-3" value={selected.includes(lead) ? lead : selected[0]} onChange={(e) => setLead(e.target.value)}>
          {selected.map((id) => <option key={id} value={id}>{agents.find((agent) => agent.id === id)?.name || id}</option>)}
        </select>
        <p className="text-xs text-muted-foreground">{tr("The lead coordinates tasks and reviews results using its configured model.", "使用队长配置的模型协调分工、检查进度和汇总结果。")}</p>
      </label>
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      <DialogFooter><Button variant="outline" disabled={saving} onClick={onClose}>{tr("Cancel", "取消")}</Button>
        <Button disabled={loading || saving || !name.trim()} onClick={() => void save()}>{saving && <LoaderCircle className="size-4 animate-spin" />}{tr("Save", "保存")}</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>;
}

// downsizeAvatar center-crops the image to a square and scales it to 128px,
// so the group picture stays a few KB inside the group settings, which the
// server reads on every group message.
async function downsizeAvatar(file: File): Promise<string> {
  const url = URL.createObjectURL(file);
  try {
    const img = await new Promise<HTMLImageElement>((resolve, reject) => {
      const el = new Image();
      el.onload = () => resolve(el);
      el.onerror = reject;
      el.src = url;
    });
    const side = Math.min(img.naturalWidth, img.naturalHeight);
    const canvas = document.createElement("canvas");
    canvas.width = canvas.height = 128;
    const ctx = canvas.getContext("2d");
    if (!ctx) throw new Error("canvas unavailable");
    ctx.drawImage(img, (img.naturalWidth - side) / 2, (img.naturalHeight - side) / 2, side, side, 0, 0, 128, 128);
    return canvas.toDataURL("image/webp", 0.85);
  } finally {
    URL.revokeObjectURL(url);
  }
}
