"use client";

import { useCallback, useEffect, useState } from "react";
import { Check, Copy, ExternalLink, FolderOpen, LoaderCircle } from "lucide-react";
import { Button } from "@/components/ui/button";
import { getMe, getStatus, revealLogs } from "@/lib/api";
import { useLocale } from "@/components/locale-provider";

const UPGRADE_CMD = "fastclaw update";
const WEBSITE_URL = "https://fastclaw.ai/?utm_source=fastclaw-app&utm_medium=about";
const REPO_URL = "https://github.com/fastclaw-ai/fastclaw";
const RELEASES_URL = `${REPO_URL}/releases`;
const LATEST_RELEASE_API = "https://api.github.com/repos/fastclaw-ai/fastclaw/releases/latest";

type UpdateState =
  | { status: "idle" | "checking" | "failed" }
  | { status: "latest" | "available" | "dev"; latest: string; url: string };

// parseVersion reads "v1.2.3" (or "1.2.3"); from-source builds such as
// "dev" or "v1.2.3-4-gabc" don't parse and are treated as dev builds.
function parseVersion(value: string): number[] | null {
  const match = value.trim().match(/^v?(\d+)\.(\d+)\.(\d+)$/);
  return match ? match.slice(1).map(Number) : null;
}

function isNewer(latest: number[], current: number[]) {
  for (let i = 0; i < 3; i++) {
    if (latest[i] !== current[i]) return latest[i] > current[i];
  }
  return false;
}

export default function AboutSettingsPage() {
  const { tr } = useLocale();
  const [version, setVersion] = useState("");
  const [deployMode, setDeployMode] = useState<"self-hosted" | "hosted" | null>(null);
  const [update, setUpdate] = useState<UpdateState>({ status: "idle" });
  const [copied, setCopied] = useState(false);
  const [isAdmin, setIsAdmin] = useState(false);
  const [logsError, setLogsError] = useState("");

  useEffect(() => {
    getStatus().then((status) => { setVersion(status.version || ""); setIsAdmin(!!status.isAdmin); }).catch(() => {});
    getMe().then((me) => setDeployMode(me.deployMode ?? null)).catch(() => {});
  }, []);

  const checkUpdate = useCallback(async () => {
    setUpdate({ status: "checking" });
    try {
      const response = await fetch(LATEST_RELEASE_API, { headers: { Accept: "application/vnd.github+json" } });
      if (!response.ok) throw new Error(String(response.status));
      const release = (await response.json()) as { tag_name?: string; html_url?: string };
      const latest = release.tag_name || "";
      const latestParts = parseVersion(latest);
      if (!latestParts) throw new Error("bad tag");
      const currentParts = parseVersion(version);
      const url = release.html_url || RELEASES_URL;
      if (!currentParts) setUpdate({ status: "dev", latest, url });
      else setUpdate({ status: isNewer(latestParts, currentParts) ? "available" : "latest", latest, url });
    } catch {
      setUpdate({ status: "failed" });
    }
  }, [version]);

  // Check once the running version is known, like a desktop app's About.
  useEffect(() => {
    if (version) void checkUpdate();
  }, [version, checkUpdate]);

  const copyCmd = async () => {
    try {
      await navigator.clipboard.writeText(UPGRADE_CMD);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard may be unavailable on insecure origins — ignore */
    }
  };

  const links = [
    { label: tr("Website", "官网"), href: WEBSITE_URL },
    { label: tr("Source code", "源码"), href: REPO_URL },
    { label: tr("Report an issue", "反馈问题"), href: `${REPO_URL}/issues` },
  ];
  // The log folder is on the server's disk, so it only opens for a
  // self-hosted super_admin (the API enforces both).
  const canOpenLogs = isAdmin && deployMode === "self-hosted";
  const openLogs = async () => {
    setLogsError("");
    const result = await revealLogs().catch((cause) => ({ ok: false, error: String(cause) }));
    if (!result.ok) setLogsError(result.error || tr("Could not open the log folder.", "无法打开日志目录。"));
  };

  const updateText = (() => {
    switch (update.status) {
      case "checking": return tr("Checking for updates…", "正在检查更新…");
      case "failed": return tr("Could not reach GitHub to check for updates.", "无法连接 GitHub 检查更新。");
      case "latest": return tr("You're on the latest version.", "当前已是最新版本。");
      case "available": return tr("{{version}} is available.", "发现新版本 {{version}}。", { version: update.latest });
      case "dev": return tr("This is a from-source build. The latest release is {{version}}.", "当前为源码构建版本，最新发布版本为 {{version}}。", { version: update.latest });
      default: return tr("Check GitHub for the latest release.", "检查 GitHub 上的最新发布版本。");
    }
  })();

  return (
    <div className="space-y-8">
      <div className="flex items-center gap-5">
        {/* eslint-disable-next-line @next/next/no-img-element */}
        <img src="/logo.png" alt="" className="size-16 shrink-0 object-contain sm:size-20" />
        <div className="min-w-0 space-y-1.5">
          <h1 className="text-xl font-semibold tracking-tight">FastClaw</h1>
          <p className="text-sm text-muted-foreground">
            {tr("The Agent factory: build, debug, and ship your Agents fast.", "Agent 制作工厂：快速制作、调试、分发你的 Agent。")}
          </p>
          <div className="flex flex-wrap items-center gap-2.5">
            <code className="font-mono text-sm text-muted-foreground">{version || tr("unknown", "未知")}</code>
            {deployMode && (
              <span className="rounded-md bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
                {deployMode === "hosted" ? tr("Cloud", "云端版") : tr("Self-hosted", "自托管版")}
              </span>
            )}
          </div>
        </div>
      </div>

      <div className="flex flex-wrap gap-x-8 gap-y-2">
        {links.map((link) => (
          <a key={link.href} href={link.href} target="_blank" rel="noopener noreferrer"
            className="inline-flex items-center gap-1.5 text-sm text-muted-foreground transition-colors hover:text-foreground">
            {link.label}
            <ExternalLink className="size-3.5" />
          </a>
        ))}
        {canOpenLogs && (
          <button type="button" onClick={() => void openLogs()}
            className="inline-flex items-center gap-1.5 text-sm text-muted-foreground transition-colors hover:text-foreground">
            {tr("Open log folder", "打开日志目录")}
            <FolderOpen className="size-3.5" />
          </button>
        )}
        {logsError && <p role="alert" className="basis-full text-xs text-destructive">{logsError}</p>}
      </div>

      <div className="border-t border-border pt-5 text-xs text-muted-foreground">
        <a href={`${REPO_URL}/blob/main/LICENSE`} target="_blank" rel="noopener noreferrer"
          className="underline-offset-4 transition-colors hover:text-foreground hover:underline">
          {tr("FastClaw Community License", "FastClaw 社区许可证")}
        </a>
        <span className="mx-2">·</span>
        © 2026 ThinkAny, LLC
      </div>

      <div className="space-y-4 rounded-xl border border-border bg-muted/40 p-5">
        <div className="flex items-center justify-between gap-4">
          <div className="min-w-0">
            <p className="text-sm font-semibold">{tr("Software update", "软件更新")}</p>
            <p className="mt-1 text-sm text-muted-foreground">{updateText}</p>
          </div>
          {update.status === "available" || update.status === "dev" ? (
            <Button variant="outline" onClick={() => window.open(update.url, "_blank", "noopener,noreferrer")}>
              <ExternalLink className="size-4" />
              {tr("View release", "查看版本")}
            </Button>
          ) : (
            <Button variant="outline" disabled={!version || update.status === "checking"} onClick={() => void checkUpdate()}>
              {update.status === "checking" && <LoaderCircle className="size-4 animate-spin" />}
              {tr("Check for updates", "检查更新")}
            </Button>
          )}
        </div>
        <div className="space-y-3">
            <p className="text-xs text-muted-foreground">
              {tr("Upgrade in place from the shell:", "在终端中运行以下命令升级：")}
            </p>
            <div className="flex items-center justify-between gap-2 rounded-md bg-background px-3 py-2">
              <code className="font-mono text-sm">{UPGRADE_CMD}</code>
              <Button size="icon" variant="ghost" className="size-7" onClick={copyCmd} aria-label={tr("Copy command", "复制命令")}>
                {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
              </Button>
            </div>
        </div>
      </div>
    </div>
  );
}
