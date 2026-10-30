"use client";

import { useCallback, useEffect, useState } from "react";
import { AlertTriangle, CheckCircle2, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { connectAgentIMessage, getAgentIMessageStatus, type IMessageStatus } from "@/lib/api";
import { useLocale } from "@/components/locale-provider";

const FULL_DISK_ACCESS_URL = "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles";
const AUTOMATION_URL = "x-apple.systempreferences:com.apple.preference.security?Privacy_Automation";

// Local iMessage: fastclaw reads this Mac's Messages database and sends
// replies through Messages.app. Two macOS permissions are involved —
// Full Disk Access (granted by hand in System Settings, then restart
// fastclaw) and Automation → Messages (a one-time prompt that appears
// when the user clicks Connect).
export function ConnectIMessageDialog({
  open,
  onOpenChange,
  agentId,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  onConnected: () => void;
}) {
  const { tr } = useLocale();
  const [status, setStatus] = useState<IMessageStatus | null>(null);
  const [checking, setChecking] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [errorCode, setErrorCode] = useState("");
  const [connected, setConnected] = useState(false);

  const check = useCallback(async () => {
    if (!agentId) return;
    setChecking(true);
    const st = await getAgentIMessageStatus(agentId);
    setStatus(st);
    setChecking(false);
  }, [agentId]);

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    getAgentIMessageStatus(agentId).then((st) => {
      if (!cancelled) setStatus(st);
    });
    return () => {
      cancelled = true;
    };
  }, [open, agentId]);

  const handleOpenChange = (v: boolean) => {
    if (!v) {
      setStatus(null);
      setSubmitting(false);
      setError("");
      setErrorCode("");
      setConnected(false);
    }
    onOpenChange(v);
  };

  const submit = async () => {
    if (!agentId) return;
    setSubmitting(true);
    setError("");
    setErrorCode("");
    const res = await connectAgentIMessage(agentId);
    setSubmitting(false);
    if (res.error || !res.ok) {
      setError(res.error || tr("Failed to connect", "连接失败"));
      setErrorCode(res.code || "");
      if (res.code === "no_disk_access") check();
      return;
    }
    setConnected(true);
    onConnected();
  };

  const needsDiskAccess = status?.available && !status.fullDiskAccess;

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <img src="/channels/imessage.svg" alt="iMessage" className="h-5 w-5 object-contain" />
            {tr("Connect iMessage", "连接 iMessage")}
          </DialogTitle>
          <DialogDescription>
            {tr(
              "This agent will answer iMessage direct messages sent to the Apple ID signed into Messages on this Mac. A dedicated Apple ID is recommended. Group chats are ignored.",
              "此 Agent 将回复发送到这台 Mac「信息」App 所登录 Apple ID 的 iMessage 私聊。建议使用专用的 Apple ID。群聊消息会被忽略。",
            )}
          </DialogDescription>
        </DialogHeader>

        {connected ? (
          <div className="rounded-lg border border-emerald-500/30 bg-emerald-500/5 p-4 space-y-2">
            <div className="flex items-center gap-2">
              <CheckCircle2 className="h-4 w-4 text-emerald-500" />
              <span className="text-sm font-medium">{tr("Connected", "已连接")}</span>
            </div>
            <p className="text-sm">
              {tr(
                "From another device, send an iMessage to this Mac's Apple ID to test. Keep this Mac awake and Messages signed in.",
                "用另一台设备给这台 Mac 的 Apple ID 发一条 iMessage 即可测试。请保持这台 Mac 不休眠、「信息」保持登录。",
              )}
            </p>
          </div>
        ) : !status ? (
          <div className="flex h-24 items-center justify-center">
            <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
          </div>
        ) : !status.available ? (
          <p className="text-sm text-muted-foreground py-2">
            {tr(
              "iMessage is only available to admins of a self-hosted fastclaw running on macOS.",
              "iMessage 仅适用于运行在 macOS 上的自部署 fastclaw，且需要管理员权限。",
            )}
          </p>
        ) : (
          <div className="space-y-3 py-2">
            <div className="rounded-lg border bg-muted/30 p-4 space-y-2">
              <div className="flex items-center gap-2">
                {needsDiskAccess ? (
                  <AlertTriangle className="h-4 w-4 text-amber-500" />
                ) : (
                  <CheckCircle2 className="h-4 w-4 text-emerald-500" />
                )}
                <span className="text-sm font-medium">
                  {tr("1. Full Disk Access", "1. 完全磁盘访问权限")}
                </span>
              </div>
              {needsDiskAccess ? (
                <>
                  <p className="text-xs text-muted-foreground">
                    {tr(
                      "fastclaw needs it to read incoming messages. In System Settings → Privacy & Security → Full Disk Access, add the program below (or the terminal app you start fastclaw from), then restart fastclaw.",
                      "fastclaw 需要它来读取收到的消息。请在「系统设置 → 隐私与安全性 → 完全磁盘访问权限」中添加下面这个程序（如果你是在终端里启动 fastclaw，则添加该终端 App），然后重启 fastclaw。",
                    )}
                  </p>
                  {status.binaryPath && (
                    <Input
                      readOnly
                      value={status.binaryPath}
                      className="font-mono text-xs"
                      onFocus={(e) => e.currentTarget.select()}
                    />
                  )}
                  <div className="flex gap-2">
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => {
                        window.location.href = FULL_DISK_ACCESS_URL;
                      }}
                    >
                      {tr("Open System Settings", "打开系统设置")}
                    </Button>
                    <Button size="sm" variant="outline" onClick={check} disabled={checking}>
                      {checking ? tr("Checking…", "正在检测…") : tr("Check again", "重新检测")}
                    </Button>
                  </div>
                </>
              ) : (
                <p className="text-xs text-muted-foreground">{tr("Granted.", "已授权。")}</p>
              )}
            </div>
            <div className="rounded-lg border bg-muted/30 p-4 space-y-2">
              <span className="text-sm font-medium">{tr("2. Allow controlling Messages", "2. 允许控制「信息」")}</span>
              <p className="text-xs text-muted-foreground">
                {tr(
                  "After you click Connect, macOS asks whether fastclaw may control Messages — click Allow. That's how replies are sent.",
                  "点击「连接」后，macOS 会询问是否允许 fastclaw 控制「信息」，请点击「允许」。回复消息需要这项权限。",
                )}
              </p>
            </div>
            {error && (
              <div className="space-y-1">
                <p className="text-xs text-destructive">
                  {errorCode === "no_automation"
                    ? tr(
                        "Permission to control Messages was denied. Enable it in System Settings → Privacy & Security → Automation, then try again.",
                        "控制「信息」的权限被拒绝。请在「系统设置 → 隐私与安全性 → 自动化」中开启后重试。",
                      )
                    : error}
                </p>
                {errorCode === "no_automation" && (
                  <a href={AUTOMATION_URL} className="text-xs underline">
                    {tr("Open Automation settings", "打开自动化设置")}
                  </a>
                )}
              </div>
            )}
          </div>
        )}

        <DialogFooter>
          {connected ? (
            <Button onClick={() => handleOpenChange(false)}>{tr("Done", "完成")}</Button>
          ) : (
            <>
              <Button variant="outline" onClick={() => handleOpenChange(false)} disabled={submitting}>
                {tr("Cancel", "取消")}
              </Button>
              {status?.available && (
                <Button onClick={submit} disabled={submitting || needsDiskAccess}>
                  {submitting ? tr("Waiting for permission…", "正在等待授权…") : tr("Connect", "连接")}
                </Button>
              )}
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
