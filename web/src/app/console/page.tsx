"use client";

import { useEffect, useState } from "react";
import { getStatus, type StatusResponse } from "@/lib/api";
import { Bot } from "lucide-react";
import { useLocale } from "@/components/locale-provider";

export default function OverviewPage() {
  const { tr } = useLocale();
  const [status, setStatus] = useState<StatusResponse | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const fetchStatus = () => {
      getStatus()
        .then(setStatus)
        .catch(() => setStatus(null))
        .finally(() => setLoading(false));
    };
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

  // The console is the caller's own account for everyone, admins
  // included; deployment-wide stats live on the System overview (/admin).
  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">{tr("Dashboard", "总览")}</h2>
        <p className="text-sm text-muted-foreground mt-1">
          {tr("Monitor your FastClaw gateway", "查看 FastClaw 网关的运行概况")}
        </p>
      </div>

      <div className="grid gap-4 grid-cols-2 md:grid-cols-2">
        <div className="rounded-lg border border-border bg-card p-5">
          <div className="flex items-center justify-between mb-3">
            <span className="text-sm text-muted-foreground">{tr("Agents", "Agent")}</span>
            <div className="flex h-8 w-8 items-center justify-center rounded-full bg-violet-500/10">
              <Bot className="h-4 w-4 text-violet-500" />
            </div>
          </div>
          <p className="text-3xl font-semibold tracking-tight">
            {status?.agents?.length || 0}
          </p>
          <p className="text-xs text-muted-foreground mt-1">{tr("Active agents", "可用 Agent")}</p>
        </div>
      </div>
    </div>
  );
}
