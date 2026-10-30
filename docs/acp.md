# Agent Communication Protocol (ACP)

FastClaw exposes its agents through the BeeAI Agent Communication Protocol
(ACP) 0.2 REST API. This lets an ACP client or another agent discover and run
the FastClaw agents allowed by an API key.

> ACP is overloaded terminology. This endpoint implements the HTTP
> **Agent Communication Protocol** for agent-to-agent calls, not the
> JSON-RPC/stdio **Agent Client Protocol** used by editors such as Zed.

## Base URL and authentication

The ACP base URL is:

```text
http://127.0.0.1:18953/acp
```

For a remote gateway, replace the origin and keep `/acp`. Send a FastClaw API
key as a bearer token on every endpoint except `GET /ping`:

```http
Authorization: Bearer fc_...
```

The API key's existing agent ACL is applied to discovery, manifests, and runs.
An agent-scoped key is the safest credential to give another agent.

## Discover agents

```bash
curl -s http://127.0.0.1:18953/acp/agents \
  -H "Authorization: Bearer $FASTCLAW_API_KEY"
```

ACP agent names must be RFC 1123 DNS labels. FastClaw IDs that contain `_` or
other unsupported characters are exposed under a deterministic ACP-safe name.
Use the returned `name` in run requests; the original ID is available at
`metadata.annotations.fastclaw_agent_id`.

Fetch one manifest:

```bash
curl -s http://127.0.0.1:18953/acp/agents/AGENT_NAME \
  -H "Authorization: Bearer $FASTCLAW_API_KEY"
```

## Run an agent

### Synchronous

`sync` is the default mode. The request waits for the completed run.

```bash
curl -s http://127.0.0.1:18953/acp/runs \
  -H "Authorization: Bearer $FASTCLAW_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "agent_name": "AGENT_NAME",
    "mode": "sync",
    "input": [{
      "role": "agent/caller",
      "parts": [{
        "content_type": "text/plain",
        "content": "Summarize the repository and identify the riskiest module."
      }]
    }]
  }'
```

The response contains a generated `session_id`. Reuse that UUID on later runs
to continue the same FastClaw conversation:

```json
{
  "agent_name": "AGENT_NAME",
  "session_id": "b5163d8f-a2ea-4dcc-8e98-285e31a1d8ec",
  "input": [{
    "role": "agent/caller",
    "parts": [{"content_type": "text/plain", "content": "Now inspect its tests."}]
  }]
}
```

### Asynchronous

Set `mode` to `async`. FastClaw returns HTTP 202 immediately. Poll the run and
inspect its recorded protocol events:

```text
GET  /acp/runs/{run_id}
GET  /acp/runs/{run_id}/events
POST /acp/runs/{run_id}/cancel
```

Run metadata is kept in memory for up to 24 hours (with a bounded 1,000-run
cache per gateway process). Conversation history itself remains in FastClaw's
normal persistent session store.

### Streaming

Set `mode` to `stream`. The response is `text/event-stream`; every SSE `data`
record is an ACP event such as `run.created`, `message.part`, or
`run.completed`.

```bash
curl -N http://127.0.0.1:18953/acp/runs \
  -H "Authorization: Bearer $FASTCLAW_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "agent_name": "AGENT_NAME",
    "mode": "stream",
    "input": [{
      "role": "user",
      "parts": [{"content_type": "text/plain", "content": "Hello"}]
    }]
  }'
```

## Content

FastClaw accepts ACP text, JSON, URL-backed attachments, and base64 content.
Image parts are both materialized into the session workspace and passed to a
vision-capable model. Other files are materialized into `/workspace` and
referenced in the agent prompt.

## Supported endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/ping` | Liveness and protocol version |
| `GET` | `/agents` | Discover accessible agents |
| `GET` | `/agents/{name}` | Get an agent manifest |
| `POST` | `/runs` | Start a sync, async, or streaming run |
| `GET` | `/runs/{run_id}` | Get run state and output |
| `GET` | `/runs/{run_id}/events` | Get recorded run events |
| `POST` | `/runs/{run_id}/cancel` | Cancel an active run |
| `POST` | `/runs/{run_id}` | Resume hook; returns conflict unless awaiting input |
| `GET` | `/sessions/{session_id}` | Get run-history links for a session |

For compatibility with the ACP 0.2 OpenAPI document, the singular
`/session/{session_id}` spelling is also accepted.
