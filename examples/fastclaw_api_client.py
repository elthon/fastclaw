#!/usr/bin/env python3
"""Minimal FastClaw backend-integration client.

This script simulates how an upstream business backend calls a tenant-owned
FastClaw Agent with an agent-scoped API key.

Usage with environment variables:

  export FASTCLAW_BASE_URL=http://127.0.0.1:18954
  export FASTCLAW_AGENT_ID=agt_xxx
  export FASTCLAW_AGENT_TOKEN=fc_xxx
  export FASTCLAW_END_USER=business-user-001
  export FASTCLAW_SESSION_ID=case-demo-001
  python3 examples/fastclaw_api_client.py "请分析这条舆情。"

For the local demo created by Codex, you can also load the generated state:

  python3 examples/fastclaw_api_client.py --state /tmp/fastclaw_integration_state.json \
    "请给出公安舆情分析的输出结构。"
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any


def load_config(state_path: str | None) -> dict[str, str]:
    cfg = {
        "base_url": os.getenv("FASTCLAW_BASE_URL", "http://127.0.0.1:18954"),
        "agent_id": os.getenv("FASTCLAW_AGENT_ID", ""),
        "agent_token": os.getenv("FASTCLAW_AGENT_TOKEN", ""),
        "end_user": os.getenv("FASTCLAW_END_USER", "demo-user-001"),
        "session_id": os.getenv("FASTCLAW_SESSION_ID", "case-demo-001"),
    }
    if state_path:
        with open(state_path, "r", encoding="utf-8") as f:
            state = json.load(f)
        cfg.update(
            {
                "base_url": state.get("base_url", cfg["base_url"]),
                "agent_id": state.get("tenant_agent_id", cfg["agent_id"]),
                "agent_token": state.get("tenant_agent_token", cfg["agent_token"]),
                "end_user": state.get("demo_external_user_id", cfg["end_user"]),
                "session_id": state.get("demo_session_id", cfg["session_id"]),
            }
        )
    missing = [k for k in ("agent_id", "agent_token") if not cfg[k]]
    if missing:
        raise SystemExit(f"missing required config: {', '.join(missing)}")
    return cfg


def request_json(method: str, url: str, token: str, body: dict[str, Any] | None = None, end_user: str | None = None) -> Any:
    headers = {
        "Accept": "application/json",
        "Authorization": f"Bearer {token}",
    }
    data = None
    if body is not None:
        data = json.dumps(body, ensure_ascii=False).encode("utf-8")
        headers["Content-Type"] = "application/json"
    if end_user:
        headers["X-Fastclaw-End-User"] = end_user
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            raw = resp.read().decode("utf-8")
            return json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"HTTP {e.code}: {raw}") from e


def iter_sse(resp):
    event_lines: list[str] = []
    for raw in resp:
        line = raw.decode("utf-8", errors="replace").rstrip("\n")
        if line.endswith("\r"):
            line = line[:-1]
        if not line:
            if event_lines:
                yield event_lines
                event_lines = []
            continue
        if line.startswith(":"):
            continue
        event_lines.append(line)
    if event_lines:
        yield event_lines


def compact_json(value: Any, limit: int) -> str:
    text = json.dumps(value, ensure_ascii=False, indent=2) if not isinstance(value, str) else value
    if len(text) <= limit:
        return text
    return text[:limit] + f"... <truncated {len(text) - limit} chars>"


def parse_json_maybe(text: str) -> Any:
    if not text:
        return ""
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        return text


def stream_chat(
    cfg: dict[str, str],
    message: str,
    params: dict[str, Any] | None = None,
    *,
    debug_events: bool = False,
    tool_output_chars: int = 800,
) -> str:
    url = urllib.parse.urljoin(cfg["base_url"].rstrip("/") + "/", "api/chat/stream")
    body = {
        "agentId": cfg["agent_id"],
        "sessionId": cfg["session_id"],
        "message": message,
        "params": params or {},
    }
    headers = {
        "Accept": "text/event-stream",
        "Content-Type": "application/json",
        "Authorization": f"Bearer {cfg['agent_token']}",
        "X-Fastclaw-End-User": cfg["end_user"],
    }
    req = urllib.request.Request(
        url,
        data=json.dumps(body, ensure_ascii=False).encode("utf-8"),
        headers=headers,
        method="POST",
    )

    final_text_parts: list[str] = []
    tool_names_by_id: dict[str, str] = {}
    print(f"agent_id: {cfg['agent_id']}")
    print(f"session_id: {cfg['session_id']}")
    print(f"end_user: {cfg['end_user']}")
    print("\n--- assistant stream ---")
    try:
        with urllib.request.urlopen(req, timeout=300) as resp:
            for event_lines in iter_sse(resp):
                data_lines = [line[5:].strip() for line in event_lines if line.startswith("data:")]
                if not data_lines:
                    continue
                payload = "\n".join(data_lines)
                evt = json.loads(payload)
                if debug_events:
                    print("\n[raw_event]")
                    print(json.dumps(evt, ensure_ascii=False, indent=2))
                event_type = evt.get("type")
                data = evt.get("data") or {}

                if event_type == "content_delta":
                    chunk = data.get("content") or data.get("delta") or ""
                    if chunk:
                        print(chunk, end="", flush=True)
                        final_text_parts.append(chunk)
                elif event_type == "content":
                    # Final full message. If no deltas were streamed, print it now.
                    content = data.get("content") or ""
                    if content and not final_text_parts:
                        print(content, end="", flush=True)
                        final_text_parts.append(content)
                elif event_type == "tool_call":
                    name = data.get("name") or data.get("tool") or "tool"
                    tool_id = data.get("id") or ""
                    if tool_id:
                        tool_names_by_id[tool_id] = name
                    args = parse_json_maybe(data.get("arguments") or "")
                    print(f"\n[tool_call] {name}" + (f" id={tool_id}" if tool_id else ""))
                    if args:
                        print(compact_json(args, tool_output_chars))
                elif event_type == "tool_result":
                    tool_id = data.get("id") or ""
                    name = data.get("name") or data.get("tool") or tool_names_by_id.get(tool_id) or "tool"
                    result = data.get("result") or ""
                    print(f"\n[tool_result] {name}" + (f" id={tool_id}" if tool_id else ""))
                    if result:
                        print(compact_json(result, tool_output_chars))
                elif event_type == "error":
                    print(f"\n[error] {data.get('message') or data}", file=sys.stderr)
                elif event_type == "done":
                    break
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"HTTP {e.code}: {raw}") from e

    print("\n--- done ---")
    return "".join(final_text_parts)


def fetch_todo(cfg: dict[str, str]) -> dict[str, Any]:
    qs = urllib.parse.urlencode({"agentId": cfg["agent_id"], "sessionId": cfg["session_id"]})
    url = urllib.parse.urljoin(cfg["base_url"].rstrip("/") + "/", f"api/chat/todo?{qs}")
    return request_json("GET", url, cfg["agent_token"], end_user=cfg["end_user"])


def main() -> int:
    parser = argparse.ArgumentParser(description="Call a FastClaw Agent via backend integration API")
    parser.add_argument("message", nargs="?", default="请用三点说明你能如何做公安舆情分析。")
    parser.add_argument("--state", help="JSON state file generated during local provisioning")
    parser.add_argument("--todo", action="store_true", help="Fetch /api/chat/todo after the chat turn")
    parser.add_argument("--debug-events", action="store_true", help="Print the full raw SSE event JSON")
    parser.add_argument("--tool-output-chars", type=int, default=800, help="Max chars to print for each tool args/result")
    args = parser.parse_args()

    cfg = load_config(args.state)
    stream_chat(
        cfg,
        args.message,
        params={"tenantId": "tenant_demo_police", "source": "python-demo-client"},
        debug_events=args.debug_events,
        tool_output_chars=args.tool_output_chars,
    )
    if args.todo:
        todo = fetch_todo(cfg)
        print("\n--- todo ---")
        print(json.dumps(todo, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
