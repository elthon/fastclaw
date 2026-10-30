# Group conversations and asynchronous topics

The implementation follows Termany's `apps/web/src/agentGroupChat.ts`,
`agentGroupDecision.ts`, `agentPrivateMessages.ts`, and their regression tests.
Coordination runs on the FastClaw server so changing conversations, closing a
view, or refreshing the browser does not stop the task.

## Execution

- Explicit `@Name`, `@agent-id`, and `@all`/`@everyone`/Chinese all-member aliases
  bypass coordination. Multiple explicit recipients execute in parallel.
- Unaddressed tasks use an isolated, tool-free call to the lead's configured
  model. The controller returns validated JSON selecting `single`, `parallel`,
  `sequential`, or `none`, exact member IDs and accessible triggering messages.
  Keyword scoring no longer selects a single member by default. Roll calls and
  requests for everyone's contribution require actual member participation;
  counting off is sequential so each member sees preceding results.
- The visible lead opens an unaddressed task when the initial dispatch starts
  with another member. The requested route then executes without losing its
  order or any handoff triggers.
- After public/private handoffs settle, the controller checks completed work
  and schedules further work or declares completion. A previous speaker can
  return to review or consolidate another member's results.
- Public mentions outside code, quoted replies, links, and email addresses
  trigger handoffs. Names use Unicode normalization and boundaries. Planned
  later speakers receive earlier handoffs in their existing scheduled turn.
- Parallel workers receive the same shared transcript. Sequential workers see
  preceding replies. Every turn includes the original request, group/member
  profiles, explicit triggers, and bounded shared history (48,000 characters,
  at most 12,000 per message). New members get the shared context too.
- Controller failures/timeouts try up to three member models, with a 30-second
  deadline per attempt. Invalid coordination never silently guesses a member.
- A failed member is quarantined for that run. Healthy members take over its
  work; parallel failures recover sequentially to avoid concurrent reuse of a
  member session. A healthy lead reports/reorganizes after failures.
- A member with no events for 90 seconds is cancelled. An execution guard pauses
  automatic discussion after 24 member attempts; the user can send a new message
  to continue. Stop cancels only the selected topic, including coordination.

## Message transport and private delivery

Member streams use a reliable execution channel; best-effort browser event
subscribers cannot drop the final reply, private delivery, or handoff. Text,
tool activity, and sender attribution remain associated with the originating
member/topic. `<|split|>` on its own line splits public bubbles outside code.

`[[private:MEMBER_ID]]body[[/private]]` routes privately to a member;
`[[private:human]]body[[/private]]` routes to the sender's separate direct inbox.
Partial delimiters, malformed blocks, and unknown/ambiguous recipients are
withheld from the public stream. Only sender and recipient receive private
bodies in their context. Controllers receive envelopes without bodies.
The human receives an unread badge and can open the direct inbox from the Agent
list. Read positions are local to the browser and authenticated user.

Images are passed to members' vision inputs; other attachments are materialized
in each member's workspace. The same topic-specific session is used when
rendering output links. Coordination receives attachment descriptions rather
than embedding raw data URLs in its prompt. Tool outputs are public activity,
not private delivery channels.

## Storage, topics and layout

The original layout is preserved: contacts on the left, conversation in the
middle, and member avatars/topics on the right (a drawer on mobile). Members
use a five-column grid. Settings expose the group name, purpose, human display
name, leader and member selection. The composer supports member autocomplete,
pasted images, and file attachments. Topics support rename/delete, running
member names, coordination phase, terminal state and round-limit state.

Authenticated topic records use the existing database `configs` store with the
separate `team_topic` kind, scoped by user and the encoded group/topic pair.
Records persist the canonical public transcript, private deliveries, title and
state. Legacy member histories are imported on the first new run. New UI reads
the canonical transcript so repeated member turns never duplicate user bubbles
or expose internal prompts/private blocks through merged history.

A restart preserves completed history/states and marks interrupted executions
stopped; it does not resume unfinished tool calls. In-flight snapshots are
checkpointed after member completion, not for every token. Multiple server
replicas still need shared execution ownership or routing to the owning process.
Members retain independent Agent sessions, mutable tool registries and prompt
builders; ordinary direct chats retain their separate session namespace.

## API

- `POST /api/chat/team/run`: accept a message, images/files; return HTTP 202 and
  a snapshot. Roster, lead and group identity come from authenticated settings.
- `GET /api/chat/team/run?teamId=…&sessionId=…`: topic snapshot, including durable
  transcript on reload (`completeHistory`).
- `GET /api/chat/team/topics?teamId=…`: topic titles and execution states.
- `POST /api/chat/team/stop?teamId=…&sessionId=…`: cancel selected execution.
- `PATCH /api/chat/team/topic?teamId=…&sessionId=…`: rename an inactive topic.
- `DELETE /api/chat/team/topic?teamId=…&sessionId=…`: remove an inactive topic;
  retain a tombstone to prevent legacy-history resurrection.
- `GET /api/chat/team/inbox`: human-directed delivery envelopes only.
- The older SSE team endpoint remains a non-owning observer.

## Validation

Go regression tests cover sequential roll calls, explicit parallel routing,
A→B→A handoffs, failover, invalid/private triggers, shared transcript bounds,
private streaming delimiters, member failure recovery, cancellation during
coordination, round limits, concurrent topics, durable restart state, user/group
isolation, and tool-free configured-model coordination.

```sh
go test ./...
go test -race ./internal/setup ./internal/agent/... ./internal/session
cd web && npm run build
```

Browser checks use mock model responses to exercise concurrent topics,
navigation between group/direct conversations, canonical-history replay,
member grids and addition, settings, mentions, attachments, and topic actions.
