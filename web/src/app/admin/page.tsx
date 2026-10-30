"use client";

import { useEffect, useState } from "react";
import {
  getStatus,
  adminListChats,
  type StatusResponse,
} from "@/lib/api";
import {
  Bot,
  Radio,
  Users,
  MessagesSquare,
} from "lucide-react";
import { useLocale } from "@/components/locale-provider";

// /admin's landing page: deployment-wide stats and configuration for
// super_admins. The per-account overview stays at /console.
export default function SystemOverviewPage() {
  const { tr } = useLocale();
  const [status, setStatus] = useState<StatusResponse | null>(null);
  const [chats, setChats] = useState<number | null>(null);
  const [loading, setLoading] = useState(true);

  const fetchStatus = () => {
    getStatus()
      .then((s) => {
        setStatus(s);
        if (s.isAdmin) {
          adminListChats()
            .then((rows) => setChats(rows.length))
            .catch(() => setChats(null));
        }
      })
      .catch(() => setStatus(null))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    fetchStatus();
    const interval = setInterval(fetchStatus, 10000);
    return () => clearInterval(interval);
  }, []);

  if (loading && !status) {
    return (
      <div className="flex h-full items-center justify-center">
        <div className="h-8 w-8 animate-spin rounded-full border-2 border-muted border-t-primary" />
      </div>
    );
  }

  // Hide empty / not-yet-connected sections so the dashboard reflects
  // what's actually configured: Channels stat when none connected.
  const channelCount = status?.channels?.length || 0;
  const showChannels = channelCount > 0;
  // Non-admins only need to see their agents — gateway plumbing (provider
  // config, users, chats) is admin-only.
  const isAdmin = status?.isAdmin ?? false;

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      {/* Header */}
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">{tr("Overview", "概览")}</h2>
        <p className="text-sm text-muted-foreground mt-1">
          {tr("Agents, users, and chats across this FastClaw deployment", "查看整个 FastClaw 部署的 Agent、用户和对话")}
        </p>
      </div>

      {/* Stats Cards — gateway-management surfaces. */}
      <div
        className={`grid gap-4 grid-cols-2 ${showChannels ? "md:grid-cols-4" : "md:grid-cols-3"}`}
      >
        {/* Agents — every agent in the deployment, not just the admin's own */}
        {isAdmin && (
          <div className="rounded-lg border border-border bg-card p-5">
            <div className="flex items-center justify-between mb-3">
              <span className="text-sm text-muted-foreground">{tr("Agents", "Agent")}</span>
              <div className="flex h-8 w-8 items-center justify-center rounded-full bg-violet-500/10">
                <Bot className="h-4 w-4 text-violet-500" />
              </div>
            </div>
            <p className="text-3xl font-semibold tracking-tight">
              {status?.totalAgents ?? "—"}
            </p>
            <p className="text-xs text-muted-foreground mt-1">{tr("Across all users", "全部用户")}</p>
          </div>
        )}

        {/* Users — admin-only */}
        {isAdmin && (
          <div className="rounded-lg border border-border bg-card p-5">
            <div className="flex items-center justify-between mb-3">
              <span className="text-sm text-muted-foreground">{tr("Users", "用户")}</span>
              <div className="flex h-8 w-8 items-center justify-center rounded-full bg-cyan-500/10">
                <Users className="h-4 w-4 text-cyan-500" />
              </div>
            </div>
            <p className="text-3xl font-semibold tracking-tight">
              {status?.users ?? 0}
            </p>
            <p className="text-xs text-muted-foreground mt-1">{tr("Registered", "已注册")}</p>
          </div>
        )}

        {/* Chats — admin-only */}
        {isAdmin && (
          <div className="rounded-lg border border-border bg-card p-5">
            <div className="flex items-center justify-between mb-3">
              <span className="text-sm text-muted-foreground">{tr("Chats", "对话")}</span>
              <div className="flex h-8 w-8 items-center justify-center rounded-full bg-amber-500/10">
                <MessagesSquare className="h-4 w-4 text-amber-500" />
              </div>
            </div>
            <p className="text-3xl font-semibold tracking-tight">
              {chats ?? "—"}
            </p>
            <p className="text-xs text-muted-foreground mt-1">{tr("Total sessions", "会话总数")}</p>
          </div>
        )}

        {/* Channels — admin-only */}
        {isAdmin && showChannels && (
          <div className="rounded-lg border border-border bg-card p-5">
            <div className="flex items-center justify-between mb-3">
              <span className="text-sm text-muted-foreground">{tr("Channels", "渠道")}</span>
              <div className="flex h-8 w-8 items-center justify-center rounded-full bg-blue-500/10">
                <Radio className="h-4 w-4 text-blue-500" />
              </div>
            </div>
            <p className="text-3xl font-semibold tracking-tight">{channelCount}</p>
            <p className="text-xs text-muted-foreground mt-1">{tr("Connected", "已连接")}</p>
          </div>
        )}

      </div>

    </div>
  );
}
