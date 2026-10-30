# FastClaw Upstream App Integration API

The integration guide for applications that use FastClaw as their agent
runtime lives in **[`skills/agent-integration/SKILL.md`](../skills/agent-integration/SKILL.md)**.
It is written for the coding agent (or developer) doing the integration and
covers:

- configuring `FASTCLAW_BASE_URL` and `FASTCLAW_API_KEY`;
- **agent-scope keys** — chatting with one or more fixed agents;
- **user-scope keys** — creating, listing, updating, deleting and chatting
  with agents on demand;
- chat sessions, end-users, files, usage, quotas and error codes.

Every FastClaw serves the same guide at `<base URL>/skills/agent-integration/SKILL.md` (public,
no key needed), and the console's **Integration** page gives a ready-to-paste
prompt for the coding agent.
