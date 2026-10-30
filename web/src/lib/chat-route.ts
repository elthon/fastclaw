"use client";

import { createContext, useContext } from "react";
import { apiFetch } from "@/lib/api";

// Conversations live at /chat/<sessionId>/, private and group chats alike.
// The URL doesn't name the agent or group, so the page resolves it from the
// session id (GET /api/chat/sessions/<id>/target). Sessions the client
// creates before the server knows them — a group topic opened before its
// first message — are remembered here so they open without a round trip.

export type ChatTarget =
  | { kind: "agent"; agentId: string }
  | { kind: "team"; teamId: string };

export type ChatRouteState =
  | { sessionId: ""; status: "none" }
  | { sessionId: string; status: "loading" | "missing" }
  | { sessionId: string; status: "ready"; target: ChatTarget };

const CACHE_KEY = "fastclaw:chat-targets";

export function chatHref(sessionId: string): string {
  return `/chat/${encodeURIComponent(sessionId)}/`;
}

// chatSessionFromPath returns the session id of a /chat/<sessionId> URL, or
// "" for anything else (including the bare /chat/ page and the static
// export's "_" placeholder).
export function chatSessionFromPath(pathname: string | null | undefined): string {
  const m = (pathname || "").match(/^\/chat\/([^/]+)\/?$/);
  if (!m || m[1] === "_") return "";
  try {
    return decodeURIComponent(m[1]);
  } catch {
    return "";
  }
}

const memory = new Map<string, ChatTarget>();

function readStored(): Record<string, ChatTarget> {
  try {
    return JSON.parse(window.sessionStorage.getItem(CACHE_KEY) || "{}") || {};
  } catch {
    return {};
  }
}

// rememberChatTarget records where a session belongs, for sessions this
// client just created or navigated to with the target in hand.
export function rememberChatTarget(sessionId: string, target: ChatTarget) {
  if (!sessionId) return;
  memory.set(sessionId, target);
  try {
    const stored = readStored();
    stored[sessionId] = target;
    window.sessionStorage.setItem(CACHE_KEY, JSON.stringify(stored));
  } catch {
    // Private mode / blocked storage: the in-memory entry still covers
    // this tab until a reload, after which the server lookup takes over.
  }
}

// newAgentChat mints a session id for a fresh private chat and records
// where it belongs, so /chat/<id>/ opens before the first message exists
// on the server — the same way a new group session does.
export function newAgentChat(agentId: string): string {
  const sessionId = `s-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  rememberChatTarget(sessionId, { kind: "agent", agentId });
  return sessionId;
}

// agentChatHref opens an Agent: its given (latest) session, or a new one
// when it has no conversation yet.
export function agentChatHref(agentId: string, sessionId?: string): string {
  if (sessionId) {
    rememberChatTarget(sessionId, { kind: "agent", agentId });
    return chatHref(sessionId);
  }
  return chatHref(newAgentChat(agentId));
}

export function knownChatTarget(sessionId: string): ChatTarget | null {
  if (memory.has(sessionId)) return memory.get(sessionId)!;
  const stored = typeof window === "undefined" ? undefined : readStored()[sessionId];
  if (stored) memory.set(sessionId, stored);
  return stored || null;
}

export async function resolveChatTarget(sessionId: string): Promise<ChatTarget | null> {
  const known = knownChatTarget(sessionId);
  if (known) return known;
  const res = await apiFetch(`/api/chat/sessions/${encodeURIComponent(sessionId)}/target`);
  if (!res.ok) return null;
  const data = await res.json();
  const target: ChatTarget | null =
    data?.kind === "agent" && data.agentId
      ? { kind: "agent", agentId: data.agentId }
      : data?.kind === "team" && data.teamId
        ? { kind: "team", teamId: data.teamId }
        : null;
  if (target) rememberChatTarget(sessionId, target);
  return target;
}

// ChatRouteContext carries the current /chat/<sessionId> resolution from
// the app shell to the chat screens, the access gate and the sidebar.
export const ChatRouteContext = createContext<ChatRouteState>({ sessionId: "", status: "none" });

export function useChatRoute(): ChatRouteState {
  return useContext(ChatRouteContext);
}
