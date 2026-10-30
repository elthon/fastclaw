import type { TeamTopic } from "@/lib/api";

type Translate = (en: string, zh: string, values?: Record<string, string | number>) => string;

export function teamProgressLabel(topic: TeamTopic | null, tr: Translate): string {
  let detail = topic?.phaseDetail;
  // Persisted runs and older servers may only have the original display text.
  // Parse that envelope, never translate member names or Agent replies.
  if (!detail && topic?.phase) {
    const match = topic.phase.match(/^(.*) 正在协调[（(](\d+)\/(\d+)[）)]$/);
    if (match) detail = { kind: "coordinating", name: match[1], attempt: Number(match[2]), total: Number(match[3]) };
    else if (topic.phase === "成员正在回复") detail = { kind: "replying" };
    else if (topic.phase === "服务重启，本轮已停止") detail = { kind: "restarted" };
  }
  switch (detail?.kind) {
    case "coordinating":
      return tr("{{name}} is coordinating ({{attempt}}/{{total}})", "{{name}} 正在协调（{{attempt}}/{{total}}）", {
        name: detail.name || tr("Coordinator", "队长"), attempt: detail.attempt || 1, total: detail.total || 3,
      });
    case "replying":
      return tr("Members are replying…", "成员正在回复…");
    case "restarted":
      return tr("Service restarted; this turn has stopped.", "服务重启，本轮已停止");
    default:
      return tr("Preparing replies…", "正在准备回复…");
  }
}
