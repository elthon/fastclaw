"use client";

import { useEffect, useMemo, useState, useSyncExternalStore } from "react";
import { Check, Copy, ExternalLink } from "lucide-react";
import { getAgents, type AgentDetail } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { useLocale } from "@/components/locale-provider";

type Scenario = "fixed" | "manage";

// The integration page hands the coding agent working on another app what
// it needs: the public agent-integration skill at /skills/agent-integration/SKILL.md
// and a prompt naming the scenario, this FastClaw's base URL and — for
// fixed agents — the agents to call. The API key stays a placeholder: the
// guide tells the agent to read it from configuration and ask for it.
export default function IntegrationPage() {
  const { tr } = useLocale();
  // window.location.origin, "" while statically prerendering.
  const origin = useSyncExternalStore(
    () => () => {},
    () => window.location.origin,
    () => "",
  );
  const [agents, setAgents] = useState<AgentDetail[]>([]);
  const [scenario, setScenario] = useState<Scenario>("fixed");
  const [picked, setPicked] = useState<string[]>([]);
  const [copied, setCopied] = useState<"" | "url" | "prompt">("");

  useEffect(() => {
    getAgents()
      .then((list) => {
        setAgents(list);
        if (list.length > 0) setPicked([list[0].id]);
      })
      .catch(() => {});
  }, []);

  const guideURL = `${origin}/skills/agent-integration/SKILL.md`;
  const keysURL = `${origin}/console/apikeys/`;
  // The prompt is for an agent, so it stays in English regardless of the
  // UI language.
  const prompt = useMemo(() => {
    const lines = [
      "Integrate this project's backend with FastClaw cloud agents.",
      "",
      `Read and follow the agent-integration skill first (install it if you support skills): ${guideURL}`,
      "",
    ];
    if (scenario === "fixed") {
      const chosen = agents.filter((a) => picked.includes(a.id));
      lines.push("Scenario A — chat with fixed agents, using an agent-scope API key. Agents:");
      if (chosen.length === 0) lines.push("- (none selected)");
      for (const a of chosen) lines.push(`- ${a.name || a.id}: agent_id ${a.id}`);
    } else {
      lines.push("Scenario B — create, manage and chat with agents on demand, using a user-scope API key.");
    }
    lines.push(
      "",
      "Configuration — server side only; ask me for any value that isn't set yet:",
      `- FASTCLAW_BASE_URL=${origin}`,
      scenario === "fixed"
        ? `- FASTCLAW_API_KEY=<an "Agent" key granted the agents above — issued at ${keysURL}>`
        : `- FASTCLAW_API_KEY=<a "User" key, on a FastClaw account dedicated to this integration — issued at ${keysURL}>`,
    );
    return lines.join("\n");
  }, [agents, guideURL, keysURL, origin, picked, scenario]);

  async function copy(kind: "url" | "prompt", text: string) {
    await navigator.clipboard.writeText(text);
    setCopied(kind);
    setTimeout(() => setCopied(""), 1500);
  }

  const togglePicked = (id: string) =>
    setPicked((l) => (l.includes(id) ? l.filter((x) => x !== id) : [...l, id]));

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">{tr("Integration", "接入文档")}</h2>
        <p className="text-sm text-muted-foreground mt-1">
          {tr(
            "Connect another app to your FastClaw agents: give the prompt below to the coding agent working on that app.",
            "把其他应用接入你的 FastClaw Agent：把下面的提示词交给负责那个应用的编程 Agent 即可。",
          )}
        </p>
      </div>

      <Card>
        <CardContent className="space-y-3 pt-6">
          <div>
            <p className="text-sm font-medium">{tr("Integration guide", "接入文档")}</p>
            <p className="text-xs text-muted-foreground mt-0.5">
              {tr(
                "Public and readable by any agent; it contains no keys. Covers configuration, fixed agents, managing agents, chat, usage and errors.",
                "公开可读，任何 Agent 都能直接读取，不含任何密钥。内容包括配置、对接指定 Agent、动态管理 Agent、对话、用量和错误处理。",
              )}
            </p>
          </div>
          <div className="flex items-center gap-2">
            <code className="flex-1 truncate rounded border bg-muted px-3 py-2 font-mono text-xs">{guideURL}</code>
            <Button size="sm" variant="outline" onClick={() => copy("url", guideURL)} title={tr("Copy", "复制")}>
              {copied === "url" ? <Check className="size-4" /> : <Copy className="size-4" />}
            </Button>
            <Button size="sm" variant="outline" onClick={() => window.open("/skills/agent-integration/SKILL.md", "_blank")} title={tr("Open", "打开")}>
              <ExternalLink className="size-4" />
            </Button>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardContent className="space-y-4 pt-6">
          <div>
            <p className="text-sm font-medium">{tr("Prompt for your agent", "给 Agent 的提示词")}</p>
            <p className="text-xs text-muted-foreground mt-0.5">
              {tr(
                "Pick how the app uses FastClaw, then paste this into the coding agent. Issue the API key on the API Keys page; the agent will ask for it.",
                "选择接入方式，然后把这段话粘贴给编程 Agent。API 密钥在「API 密钥」页签发，编程 Agent 会向你要。",
              )}
            </p>
          </div>

          <div className="grid gap-2 sm:grid-cols-2">
            <ScenarioOption
              active={scenario === "fixed"}
              onClick={() => setScenario("fixed")}
              title={tr("Use specific agents", "对接指定 Agent")}
              description={tr(
                "Chat with one or more existing agents. Key type: Agent, granted those agents.",
                "与一个或多个现有 Agent 对话。密钥类型：Agent，授权这些 Agent。",
              )}
            />
            <ScenarioOption
              active={scenario === "manage"}
              onClick={() => setScenario("manage")}
              title={tr("Manage agents", "动态管理 Agent")}
              description={tr(
                "Create, update, delete and chat with agents from the app. Key type: User, ideally on a dedicated account.",
                "由应用创建、修改、删除 Agent 并对话。密钥类型：用户，建议使用专用账号。",
              )}
            />
          </div>

          {scenario === "fixed" && (
            <div className="space-y-1.5">
              <p className="text-xs font-medium">{tr("Agents", "Agent")}</p>
              {agents.length === 0 ? (
                <p className="text-xs text-muted-foreground">
                  {tr("No agents yet — create one on the Agents page first.", "还没有 Agent，请先在 Agent 页面中创建。")}
                </p>
              ) : (
                <div className="flex flex-wrap gap-2">
                  {agents.map((a) => (
                    <button
                      key={a.id}
                      type="button"
                      onClick={() => togglePicked(a.id)}
                      className={
                        "rounded-md border px-2.5 py-1 text-xs transition " +
                        (picked.includes(a.id)
                          ? "border-primary bg-primary/10 text-primary"
                          : "border-border hover:bg-muted")
                      }
                    >
                      {a.name || a.id}
                    </button>
                  ))}
                </div>
              )}
            </div>
          )}

          <div className="relative">
            <pre className="whitespace-pre-wrap rounded border bg-muted px-3 py-2 pr-12 font-mono text-xs leading-relaxed">
              {prompt}
            </pre>
            <Button
              size="sm"
              variant="outline"
              className="absolute right-2 top-2"
              onClick={() => copy("prompt", prompt)}
              title={tr("Copy", "复制")}
            >
              {copied === "prompt" ? <Check className="size-4" /> : <Copy className="size-4" />}
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

function ScenarioOption({
  active,
  onClick,
  title,
  description,
}: {
  active: boolean;
  onClick: () => void;
  title: string;
  description: string;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={
        "rounded-md border px-3 py-2 text-left transition " +
        (active ? "border-primary bg-primary/10" : "border-border hover:bg-muted")
      }
    >
      <span className="text-sm font-medium">{title}</span>
      <p className="mt-1 text-xs text-muted-foreground">{description}</p>
    </button>
  );
}
