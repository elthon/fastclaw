import { getAgents } from "@/lib/api";
import { agentChatHref, chatSessionFromPath, resolveChatTarget } from "@/lib/chat-route";

// FastClaw's web UI has three areas, switched from the left rail (AppRail):
// the chat (the official FastClaw client), /console (the account's own
// agents, models, skills, keys) and, for super_admins, /admin. We remember
// the last page of each area so switching back resumes it, and which area
// the user was last in so signing in returns there; chat is the default.

export type Area = "chat" | "console" | "admin";

const MODE_KEY = "fastclaw:last-mode";
const LAST_KEY: Record<Area, string> = {
  chat: "fastclaw:last-chat",
  console: "fastclaw:last-console",
  admin: "fastclaw:last-admin",
};

const CHAT_ROUTE = /^\/(?:chat\/[^/]+\/?$|agents\/[^/]+\/(?:chat|project)(?:\/|$)|teams\/[^/]+\/chat\/)/;

// areaOf maps a pathname to its area, or null for pages outside all three
// (settings, onboarding, legacy redirects).
export function areaOf(pathname: string): Area | null {
  if (CHAT_ROUTE.test(pathname) || /^\/agents\/[^/]+\/?$/.test(pathname)) return "chat";
  if (/^\/console(?:\/|$)/.test(pathname)) return "console";
  if (/^\/admin(?:\/|$)/.test(pathname)) return "admin";
  return null;
}

function read(key: string): string {
  try {
    return window.localStorage.getItem(key) || "";
  } catch {
    return "";
  }
}

function write(key: string, value: string) {
  try {
    window.localStorage.setItem(key, value);
  } catch {
    // Private mode / blocked storage: landing just falls back to defaults.
  }
}

// rememberLocation records the current page as the place to resume.
// Read-only audit views (?actAs=) are someone else's chats and are skipped.
export function rememberLocation(pathname: string, search: string) {
  if (!pathname || search.includes("actAs=")) return;
  const area = areaOf(pathname);
  if (!area) return;
  write(MODE_KEY, area);
  write(LAST_KEY[area], pathname + search);
}

// resolveChatLanding returns the last conversation, or the newest agent's
// chat; accounts without agents go to the console to create one.
export async function resolveChatLanding(): Promise<string> {
  const agents = await getAgents().catch(() => []);
  if (agents.length === 0) return "/console/agents/";
  const lastChat = read(LAST_KEY.chat);
  const lastAgent = lastChat.match(/^\/agents\/([^/]+)\//)?.[1];
  // Agent chats only resume while the agent still exists for this user;
  // group chats re-check access on their own screen.
  // A /chat/<id> the server doesn't know (a new chat left before its first
  // message, or one since deleted) isn't resumed.
  const lastSession = chatSessionFromPath(lastChat.split("?")[0]);
  const lastSessionOk = !lastSession || !!(await resolveChatTarget(lastSession).catch(() => null));
  if (lastChat && lastSessionOk && (!lastAgent || agents.some((a) => a.id === decodeURIComponent(lastAgent)))) {
    return lastChat;
  }
  return agentChatHref(agents[0].id);
}

// areaHome is where switching to a management area lands: its last page,
// or its root.
export function areaHome(area: "console" | "admin"): string {
  return read(LAST_KEY[area]) || (area === "console" ? "/console/" : "/admin/");
}

// resolveAppLanding picks the first page after sign-in: the area the user
// was last in (chat by default).
export async function resolveAppLanding(): Promise<string> {
  const mode = read(MODE_KEY);
  if (mode === "console" || mode === "admin") {
    const agents = await getAgents().catch(() => []);
    if (agents.length === 0) return "/console/agents/";
    return areaHome(mode);
  }
  return resolveChatLanding();
}
