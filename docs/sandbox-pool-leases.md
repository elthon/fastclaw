# Cross-pod E2B sandbox lease registry

> **Status**: implemented, **unreleased** (built on the
> `feat/e2b-sandbox-leases` line, now part of `fastagent`)
> **Storage**: Postgres (`sandbox_leases`) in production; sqlite in tests
> **Last updated**: 2026-09-14
> **Decision owner**: mengmengmengqiang@gmail.com
> **Reviewed by**: mengmengmengqiang@gmail.com (2026-09-09)
> **Commits**: the `internal/sandbox` history on `fastagent`. Two rounds are
> documented below: rebuild-publish (2026-09-13) and the provider lifecycle
> redesign (2026-09-14, stages 1–4 shipped; stage 5 implemented and withdrawn)
> **Open follow-ups**: one — measure how fast paused instances accumulate in
> production and decide from that number whether stage 5 comes back (see
> "Orphan reaping: shipped, withdrawn, and the trigger to bring it back").
> Every clause of the invariant below has a guarding mechanism and a test.
> Rotation runbook:
> [sandbox-secret-rotation.md](./sandbox-secret-rotation.md).

## Problem

The sandbox pool is per gateway process. With N gateway replicas behind a
round-robin service, one (agent, project, session) can be served by every pod
in turn, and each pod lazily creates its **own** e2b instance for the same
scope → up to N sandboxes per session (observed: 10 pods → 10 e2b instances).

## Decision

Share the scope→sandbox mapping in Postgres. Each row is a lease:

```text
scope_key      PRIMARY KEY — "agent[:p:proj][:s:sess]" (poolKey)
owner          pod identity ("hostname:pid")
sandbox_id     e2b instance id
envd_token     short-lived e2b access token (shared so other pods can adopt)
template       e2b template used at creation
state          "running" | "paused" — advisory (see the invariant's I clause)
paused_at      unix seconds; 0 while running
expires_at     unix seconds; expired ⇒ dead, next acquirer may replace
epoch          fencing version; bumped on renew/adopt/replace, reset to 1
               by a fresh acquire (monotonic within a lease cycle, not across)
updated_at     unix seconds
```

## Alternatives considered

- **No sharing (status quo)** — each pod creates its own E2B instance per
  scope. Rejected: this is the problem (N pods → N instances, observed with
  10 replicas), and retries/crash recovery only make it worse.
- **Sticky/affinity routing** — pin a session to one pod so only that pod
  creates its sandbox. Rejected: load imbalance, pod loss still strands or
  duplicates instances, and it requires router changes that do not fix the
  underlying correctness question (who may destroy an instance).
- **Redis lease/lock** — rejected: Redis is an optional dependency in this
  codebase (refresh locks, pub/sub), so making leases require it would break
  single-node/sqlite deployments; and a Redis lock would still need
  hand-rolled fencing (a versioned token) to match the CAS guarantees the
  relational table provides. The lease row also belongs next to the rest of
  the per-agent state in the same store.
- **Reuse the workspace blob store** — rejected: it is an artifact store
  without transactional compare-and-set; lease ownership needs CAS semantics,
  which the relational table provides.

## Invariant

The design serves one contract. It is written as three **separately
falsifiable** clauses so that a violation can be attributed to a clause
instead of argued about as a whole. "Scope" means one `poolKey` —
`agent[:p:proj][:s:sess]`.

| # | Clause | Violated when |
|---|---|---|
| **U** · unique | At most one **live** sandbox serves a scope | a second live instance for that scope exists |
| **A** · available | A scope can be served without paying a rebuild on every call | the row keeps naming a dead instance, or no instance can be produced at all |
| **I** · isomorphic | The row names the instance the serving executor actually holds | in-memory identity ≠ `sandbox_id` |

Every path through the pool is one of five operations — create, adopt, rebuild,
release, expiry — so each clause can be checked operation by operation:

| Clause | Enforced by | Accepted window | Known open violators |
|---|---|---|---|
| U | the row is the only naming authority: single-winner `Acquire`, the loser closes its own copy, adoption is a CAS, destroy is fenced on `owner`+`epoch` **and** reads the control-plane answer (a rejected DELETE is an error, 404 is "already gone"); per-scope locks make `Get`/`Release` single-writer for a scope, and within one executor `rebuildMu` funnels parallel rebuilds into a single replacement | two pods may briefly share one instance during a takeover; a lapsed lease is reclaimable at TTL while its instance lives on until the provider timeout | none known |
| A | per-use reconcile; rebuild only on a **status-code** 502/404 from envd (any other failure surfaces instead of costing an instance); fail-open keeps the local sandbox serving; a rebuild that cannot hydrate or verify destroys its replacement and restores the previous identity, so the executor never keeps a sandbox that cannot serve | a publish lands on the next `Get`, so the row may lag the executor by one call | none known |
| I | the identity (`id` + token + pending-publish bit) is one value behind one mutex, swapped whole; `ReplaceSandboxLease` moves the row under an `owner` CAS; the expiry re-acquire stamps the current id | the pending bit is in-memory only: a crash before the next `Get` loses it and the row ages out via TTL | none known |

Two properties keep the clauses honest rather than aspirational:

- **`epoch` is monotonic within a lease cycle only** — a fresh `Acquire` resets
  it to 1. It fences destroys inside a cycle; a delayed destroy from an earlier
  cycle by the same owner is not covered.
- **The lease TTL (15 min) is shorter than the e2b instance lifetime (30 min)**,
  so "lease expired" means *unowned*, not *gone*. Both accepted windows above
  follow from that gap.

The open violators carry no test name on purpose: by construction they have
none, which is how they stayed open. Accepted windows and registry failure
modes each map to a test in the failure-semantics table below.

## Semantics (v1, deliberately small)

- A pod handling a scope first looks up a valid lease → **adopts** the
  existing sandbox (no new e2b create), renews ownership to itself.
- No valid lease → create + hydrate locally, then `AcquireSandboxLease`.
  A lost race (another replica just won) closes the local copy and adopts the
  winner's sandbox.
- Before every use of a cached executor the pool reconciles against the
  shared lease and renews under this pod's ownership (TTL default 15 min)
  when the sandbox is unchanged.
- Release/eviction deletes the lease only when `owner = this pod` **and** the
  `epoch` this pod last received still matches (fenced delete). If another
  pod adopted in between, the evicting pod drops its local reference
  **without** destroying the shared sandbox. Registry errors fail open: the
  sandbox is left alive.
- Adoption does not replay hydration (creator hydrated the same scope);
  skill/workspace changes apply on next recreate, same as single-pod behavior.
- **Adoption carries the account API key.** The row stores only
  `sandbox_id` + `envd_token`, and exec/read/write authenticate with the envd
  token alone — so an adopted sandbox serves normally while its key is absent.
  It breaks at the first operation that needs the *account* key: `recreate()`
  minting a replacement (e2b answers `401 authorization header is missing` for
  an empty `X-API-Key`) and `Close()` destroying one. The key therefore travels
  from the pool into every executor it hands out, adopted ones included.
- **A rebuild moves the lease.** `recreate()` replaces the instance the row is
  supposed to name, so the pool overwrites the row onto the replacement:
  `ReplaceSandboxLease` CASes on `owner` and bumps `epoch`. This is a distinct
  write because neither existing one can express it — `Renew` CASes on the OLD
  `sandbox_id` (so it can only maintain the status quo), and
  Release-then-Acquire opens a window where a sibling replica sees a free
  scope and creates a third sandbox for it.
- **Routine expiry pauses; activity resumes.** Sandboxes are created with
  `autoPause` + `autoPauseMemory` + `autoResume` (`e2bCreateBody`), so the
  30-minute timeout yields a paused instance — full memory snapshot, running
  processes included — that the next request wakes. Paused instances are not
  billed, do not count toward the concurrency limit, and are kept indefinitely
  ([auto-resume](https://docs.e2b.dev/sandbox/auto-resume.md),
  [paused + concurrency](https://docs.e2b.dev/faq/paused-sandboxes-concurrency.md)).
  That keeps an ordinary expiry off the failure path: without it the same event
  arrives as `502 sandbox not found` and costs a create, a re-hydrate and an
  identity re-publication. The rest of that redesign is staged below.
- **Sandboxes are created `secure`.** `envdAccessToken` is issued only for
  sandboxes created with `secure: true`; otherwise it is null and "envd
  endpoints work without auth", while `network.allowPublicTraffic` defaults to
  true ([create-sandbox](https://docs.e2b.dev/api-reference/sandboxes/create-sandbox.md)).
  Without it, anyone who holds a sandbox id can run commands in the sandbox, and
  the id is not a secret we keep — the runtime mints preview URLs from it
  (`ExposePort`), the row stores it, the logs print it. The create body now sets
  `secure: true`, so envd requires the token.
- **A superseded token is a repair, not a rebuild.** A secure sandbox's token can
  change across a pause, and envd answers **401** when it no longer accepts the
  one we hold. That is classified separately from "gone": the executor calls
  `connect` (which resumes if needed, extends the TTL, and returns the current
  token from the `Sandbox` response), records the new token as pending, and
  retries once. The row is updated by the next reconcile through the same
  publish path a rebuild uses, so no caller has to know the token changed.
  `e2b_token_probe_test.go` (build tag `manual`, credentials required) settles
  the empirical half — whether the create-time token would have survived a
  pause+resume anyway — which decides whether that refresh is load-bearing or
  merely tidy.
- **Idle eviction puts a sandbox to sleep.** When the backend can pause
  (`ScopeSleeper`, implemented by the E2B pool), the idle sweep pauses the
  instance and renews the row instead of releasing it, so the scope keeps the
  same `sandbox_id` and the next caller resumes it; the `hydrated` flag is kept
  because a paused sandbox still holds its filesystem. Two rules make this safe
  rather than destructive: a scope with an operation in flight is never
  considered idle (`inUse`), and a sleep that fails for any reason other than
  "the instance is already gone" leaves the sandbox **running** — a provider
  that cannot pause must not turn into a destroy.
  The `paused` annotation is written **after** the lease renew, not before: the
  renew reconciles, and reconciling a pending rebuild publishes the replacement
  — which stamps the row `running`, because a rebuilt instance is running by
  definition. The other order would leave the row describing a sleeping sandbox
  as awake. The annotation carries the same owner + liveness CAS as the other
  writes, so a pod that lost the scope (or a row that has since expired) cannot
  be annotated at all.

### Failure semantics (registry errors fail open)

Fail-open means "the sandbox is left alive", but each operation degrades
differently. This table is the contract; each row maps to its test.

Guiding principle: **availability first, destruction right strictly gated**
— without proof of ownership (a matching epoch) a pod must never close a
sandbox, even if that means leaking one until TTL/expiry.

| Registry condition | Pool behavior | Test |
|---|---|---|
| No registry failure (baseline) | Acquire/renew/adopt succeed with a fresh epoch; a rebuild moves the row onto the replacement and does not close it; Release with the matching epoch deletes the row and closes the sandbox exactly once | `TestSandboxLeasesAcquireAdoptRenewRelease`, `TestE2BPoolReleaseUsesFencingEpoch`, `TestE2BPoolReconcileRepublishesRebuiltSandbox` |
| Token decrypt failure on read (rotated / mismatched key) | Read fails closed → pool treats it as a lookup error and creates locally; an unexpired row blocks registration until TTL | `TestEncryptedSandboxLeaseStoreRotation` |
| Token encrypt failure before write | Refuses to persist (never writes plaintext); the error propagates to the pool's acquire-error path, which keeps the local sandbox unregistered | pool acquire-error path: `TestE2BPoolFreshGetLeaseErrorsFailOpen` |
| Lookup error before local create | Still creates + registers locally; an acquire error keeps it unregistered | `TestE2BPoolFreshGetLeaseErrorsFailOpen` |
| Adopt renew error | Uses the adopted executor but records **no epoch**, so release can never destroy it | `TestE2BPoolAdoptRenewErrorKeepsExecutorWithoutEpoch` |
| Reconcile lookup error (cached) | Keeps the cached executor; no renew, no close | `TestE2BPoolReconcileRegistryErrorsKeepLocal` |
| Reconcile reclaim acquire error | Keeps the cached executor, unregistered | `TestE2BPoolReconcileRegistryErrorsKeepLocal` |
| Double race (Acquire lost + adoption CAS miss) | Keeps the local sandbox unregistered until the next reconcile | `TestE2BPoolCreateLostRaceAdoptMissKeepsLocalUnregistered` |
| Rebuild publish, registry error | Keeps serving from the rebuilt sandbox; the `rebuilt` flag stays set and the next reconcile retries the publish | `TestE2BPoolRebuildPublishFailureKeepsLocalAndRetries` |
| Rebuild publish, CAS miss (scope moved to another pod) | Does **not** overwrite the winner's row; falls through to adopting the current lease and closes its own replacement exactly once | `TestE2BPoolRebuildSupersededByAnotherPodAdoptsCurrent` |
| Rebuild with no shared lease (single pod / docker) | Nothing to publish; `recreate()` behaves exactly as before | `TestE2BPoolRebuildWithoutLeaseStoreIsInert` |
| Rebuild whose hydrate/verify fails | Destroys the unusable replacement, restores the previous identity, surfaces the error; nothing is published and the next reconcile does not adopt the dead sandbox back | `TestE2BExecutorFailedRebuildRestoresIdentityAndDestroysReplacement` |
| Create accepted but the edge cannot route the id yet | `waitUntilRoutable` retries while envd answers 502/404 (1.5s interval, 60s bound) and fails creation naming the sandbox if it never comes up; a verdict that is not "gone" (401 from a stale token, 500 inside the sandbox) fails immediately without retrying; an instance that never becomes routable is destroyed rather than leaked | `TestE2BWaitUntilRoutable` |
| Hydrate hits a cut stream or a 502 right after create | Retries the network steps up to 3 times, 1.5s apart — a container that was created moments ago can cut one stream while it finishes booting. Only "not routable yet" is retried: a 401 or a permission error is a verdict and fails immediately. The bundle is built once, outside the loop | `TestHydrateRetriesATruncatedStream`, `TestHydrateDoesNotRetryAVerdict` |
| Idle sweep, backend can pause | Pauses the instance, renews the row (the paused instance is free to keep, so the row that names it should survive too) and keeps `hydrated` — the sandbox still holds its filesystem. A scope with an operation in flight is skipped entirely | `TestLifecycleIdleSleepsInsteadOfReleasing`, `TestLifecycleDoesNotEvictWhileAnOperationRuns` |
| Idle sweep, sleep fails (a provider without pause, a transient API error) | Leaves the sandbox running, logs a warning, and keeps the scope in the idle set so the next sweep retries — dropping it would leave a running, billed sandbox that nothing tracks. Only "there was nothing to sleep" falls through to the normal release | `TestLifecycleKeepsSandboxItCouldNotSleep`, `TestAFailedSleepKeepsTheScopeInTheSweepSet`, `TestLifecycleFallsBackToReleaseWhenThereIsNothingToSleep` |
| Operation longer than the sandbox's remaining TTL — **both clocks** | Before an operation of ≥ 60s, `ScopeExtender` moves the instance's expiry to `budget + 2 min` (so the auto-pause cannot land mid-operation and cut the stream) **and** renews the lease for the same budget (so the row cannot lapse mid-operation, which would let a sibling replica acquire the scope and start a second sandbox while this one is still working). Renewing never shortens below the pool's own TTL. Shorter operations may still straddle an expiry seconds away; that surfaces as a truncated stream and the caller can retry | `TestLifecycleExtendsTheSandboxBeforeALongOperation`, `TestE2BExtendTimeoutMovesTheExpiry`, `TestExtendScopeMovesTheLeaseClockToo` |
| envd answers 401 (the token was superseded, e.g. across a pause) | Reconnects for the current token, records it as pending publication (the next reconcile writes it to the row through the rebuild-publish path) and retries once — the sandbox and its id are untouched | `TestE2BRefreshesASupersededEnvdToken`, `TestStaleEnvdTokenClassification` |
| Adopting a scope whose row says `paused` | Calls `connect` before use: it resumes the instance, extends its TTL and returns the current token, so the first call does not pay a 401 first. Marked `running` afterwards. A running row skips this entirely | `TestAdoptingAPausedSandboxResumesIt`, `TestAdoptingARunningSandboxDoesNotConnect` |
| Parallel rebuilds on one executor | The first caller replaces the sandbox; the rest observe the new identity and retry on it without creating anything — exactly one instance per dead sandbox | `TestE2BExecutorConcurrentRebuildMintsOneSandbox` |
| envd failure that is **not** 502/404 (500 inside the sandbox, 401 from a stale token) | Surfaces to the caller; no rebuild — a rebuild cannot fix it and would cost an instance | `TestE2BExecDoesNotRebuildOnNonGoneFailures` |
| Destroy answer: 2xx / 404 / anything else | 2xx and 404 succeed (a 404 means the instance is already gone, which is the goal); any other status is returned as an error naming the sandbox, so a "released" sandbox cannot keep running unnoticed | `TestE2BCloseReadsTheAnswer` |
| Release / CloseAll registry error or declined delete | Drops the local reference; the sandbox stays alive | `TestE2BPoolReleaseRegistryErrorLeavesSandboxAlive`, `TestE2BPoolCloseAllHonorsLeaseStore` |

## Hardening: CAS + epoch (stage 2)

Cross-pod correctness needs the ownership transfer itself to be a
compare-and-set, not a read-then-write:

- Every successful renew/adopt increments `epoch` and only matches
  `scope_key + sandbox_id + expires_at > now`. A stale renew (row deleted or
  pointing at another sandbox) returns no row and must not be treated as
  ownership.
- Destroy is only allowed after
  `DELETE … WHERE scope_key=? AND owner=? AND epoch=?` returns one row. The
  evicting pod carries the epoch it last received, so a delayed/duplicated
  eviction from an older snapshot can never delete a lease that was renewed
  or adopted in the meantime.
- `Acquire` claims a fresh/expired row and stamps `epoch = 1`.

Why epoch on top of owner: owner changes cover most takeovers, but not
same-owner request reordering (an old release racing a newer renew from the
same pod) or a delete formulated before another pod's adoption completed.
The version column makes any stale destroy request fail closed.

### Staged hardening plan

| Stage | Scope | Status |
|---|---|---|
| 1 | lease table + adopt/acquire/release + gateway wiring | done (`e359bf0`) |
| 1b | per-use reconcile: cached executor vs lease sandbox_id | done (`091c579`) |
| 2 | CAS + epoch on renew/adopt/release + race unit tests | done (`86fcac1`) |
| 2b | rebuild republish: key travels into adopted executors; `ReplaceSandboxLease` moves the row onto a rebuilt instance | done (2026-09-13) |
| 3–6 | heartbeat/reconciliation loop, degraded state machine, metrics, liveness GC | **not in scope** — decision: CAS + epoch is sufficient for the current release; revisit only if prod observations justify them |

### Provider-lifecycle redesign (staged, 2026-09-14)

The e2b primitives above move routine expiry off the failure path, so the stages
this document had frozen become cheap and concrete. Order, and what each one
buys:

| # | Change | Status |
|---|---|---|
| 1 | `autoPause` + `autoPauseMemory` + `autoResume` on create (`e2bCreateBody`) | **done** — an expiry pauses and the next request resumes it, instead of costing a rebuild |
| 2 | Idle eviction **pauses** instead of releasing (`ScopeSleeper`), behind the in-flight guard; a sleep that fails for any reason but "already gone" leaves the sandbox running | **done** |
| 3 | `set-timeout` before an operation long enough to outlive the instance TTL, so an auto-pause never lands mid-exec | **done** — `ScopeExtender` → `ExtendTimeout`, called for budgets ≥ 60s with `budget + 2 min` |
| 4 | `state` / `paused_at` on the row; adopting a paused instance resumes it and refreshes the token when needed | **done** — columns + idempotent retrofit (`migrateSandboxLeasesAddState`), the idle sweep writes `paused`, adoption reads it and `connect`s (fresh token included), and a 401 anywhere still reconnects. The marker is advisory: it is never the basis of a destroy decision, because traffic can wake a paused sandbox without writing to the row |
| 5 | Reconcile the table against provider lifecycle events (webhook or polled), including killing paused instances no row names | **withdrawn by decision (2026-09-14)** — an implementation existed and was removed unshipped; the requirement is being settled by measurement instead, see below |

Facts that size the design: paused sandboxes are unbilled, outside the
concurrency limit and kept indefinitely; continuous runtime is capped per plan
(Hobby 1h, Pro 24h) and resets on pause+resume; concurrent *running* sandboxes
are 20 (Hobby) / 100 (Pro, add-on to 1,100) — [billing](https://docs.e2b.dev/billing.md).

### Orphan reaping: shipped, withdrawn, and the trigger to bring it back

Stages 1–4 leave one hole open on purpose. Pausing instead of destroying is what
makes the next call a resume, but nothing ends an instance any more except us, and
the provider will not pick up the slack: a paused sandbox has **no time-to-live
and is never deleted automatically** —

> "Paused sandboxes are kept **indefinitely**; there is no automatic deletion or
> time-to-live limit … There is currently **no configurable 'auto-kill after N
> days' option**; a paused sandbox will not expire on its own. To remove a paused
> sandbox, you must kill it explicitly."
> — [sandbox persistence](https://docs.e2b.dev/sandbox/persistence)

> "**How long a sandbox can stay paused** is not capped at all."
> — [How long does a sandbox live?](https://docs.e2b.dev/faq/sandbox-lifetime)

Two more facts from the same pages, because they are easy to get backwards: the
running timeout ends the *running* phase only — with `onTimeout: "pause"`, which
is what we ask for, the sandbox becomes paused rather than terminated — and the
continuous-runtime cap (1h Hobby / 24h Pro) **resets** on pause+resume, so it
never bounds how long an instance exists. `DELETE` does apply to a paused
sandbox: "A paused sandbox is only removed when you explicitly call `kill` on
it" ([paused sandboxes and concurrency](https://docs.e2b.dev/faq/paused-sandboxes-concurrency)).

So every scope nobody uses again — a normal idle session, a crashed pod, a row a
takeover replaced, a rotation that made a row unreadable — leaves an instance in
the account permanently. It is unbilled and outside the concurrency limit, which
is why this is a bookkeeping problem rather than a cost one.

**A reaper was implemented and then withdrawn before release** (commit
`56bdb38`, reverted in the commit that carries this text). Its rule was three
conjuncts, all needed: the provider must report `paused`, the instance's metadata
must carry this deployment's pool tag (written at create time, so it outlives the
row), and no lease row may still claim it — where a lapsed row keeps protecting
its instance for two lease TTLs, because the scope can still come back inside
that window. A tag was required because an e2b account can be shared, and
"paused and unclaimed" describes another deployment's live sandbox just as well
as our orphan.

It was withdrawn for one reason: the requirement rests on growth that has never
been measured. **Observation before automation** — we deploy stages 1–4, watch
the account, and only then decide. What to record:

```bash
# paused instances this deployment created (needs a tag; without one, count by
# template and cross-check against the lease table)
curl -s -H "X-API-Key: $E2B_API_KEY" \
  'https://api.e2b.app/v2/sandboxes?state=paused&limit=100' | jq 'length'

# rows that lapsed and can no longer serve: the shape of what a reaper would find
psql -c "SELECT count(*) FROM sandbox_leases WHERE expires_at < extract(epoch from now())"
```

Record both once a day for a week, alongside the active-session count, and read
the trend per session rather than the absolute number. The trigger to bring the
reaper back is any of:

- paused instances grow without settling (roughly: one per abandoned scope, so
  the count tracks historical session count, not current load);
- e2b documents or a support answer states an account-level cap on paused
  sandboxes — that turns this from housekeeping into an availability risk;
- the dashboard or `GET /v2/sandboxes` becomes impractical to audit by hand.

If none of those happens, leaving it withdrawn is the right call: the alternative
to reaping is not "the provider cleans up", it is "we clean up by hand", and the
measurement is what tells us how often.

One commit-history note so a bisect lands on the right story: step 2's
`LeaseRenewer` hook (`E2BExecutorPool.RenewLease`) actually shipped a commit
earlier, inside the pause-on-timeout change, and was wired up here. The pause
commit's message does not mention it. It is a harmless unused method at that
point in the history, and it was left in place rather than rewriting the
branch — at the time of writing there were 14 commits after it, seven of them
another agent's, on a branch still being written to.

### Stage 2b: how a rebuild reaches the lease

A sandbox that idles out is detected by an envd call returning `502`/`404`,
and `recreate()` replaces it in place. That changes the identity the lease is
supposed to name, so something has to move the row. The mechanism is
deliberately small:

1. `recreate()` sets an in-memory `rebuilt` flag on the executor. The pool owns
   the lease, so the executor records only the fact — it never sees SQL, the
   scope key, or the epoch.
2. The next `Get` reconciles as usual. A row whose `sandbox_id` differs from
   the executor's normally means "another replica took the scope over" and the
   pool adopts it. The `rebuilt` flag is what separates that from "this row
   names a sandbox I replaced myself" — the two are indistinguishable from the
   row alone, and reading the first as the second is what closed the healthy
   replacement and adopted the corpse back on every call.
3. `ReplaceSandboxLease` overwrites the identity in place, CAS on `owner`, and
   bumps `epoch`; the new epoch is recorded so a later fenced delete stays
   valid. On a registry error the flag stays set and the next reconcile
   retries; on a CAS miss the scope belongs to another pod and the pool
   adopts that one instead.

**Lifecycle of the flag** — set by a successful `recreate()`, cleared as soon
as the row names the replacement, whichever route got there first:

| Route | When | Write |
|---|---|---|
| Publish | row names a different sandbox and this pod rebuilt it | `ReplaceSandboxLease` (CAS on owner) |
| Expiry re-acquire | row lapsed (`rec == nil`) while the pod was idle | `AcquireSandboxLease` stamps `ex.sandboxID`, so the replacement is named by construction |

It is in-memory only: nothing is persisted about "a rebuild happened", so a
pod that rebuilds and then restarts before its next `Get` leaves the row on
the dead sandbox and the normal TTL takeover applies.

**Concurrency** — parallel tool calls share one executor (the agent loop fans
them out and only `delegate_task` is registered serial), so several goroutines
can watch the same sandbox die. Two guards keep that from fragmenting the
scope: `rebuildMu` serialises rebuilds and each caller reports the identity it
saw fail, so every waiter but the first finds the replacement already in place
and returns without creating anything; the identity itself travels as one value
behind one mutex, so a request can never be assembled from a new sandbox id and
the previous token.

The pool adds a third, coarser one: `Get`/`Release` serialize **per scope**
(64 striped locks), not process-wide. Everything in that path does network I/O
— lease reads and writes, create, hydrate, verify, warmup (bounded at 120s) —
and an earlier version held a single `p.mu` across all of it, so one cold scope
stalled sandbox binding for every other agent. `p.mu` now guards only the
executor/epoch maps, every critical section on it being a map lookup or
assignment. Same-scope callers still queue on the same stripe, which is what
preserves "one sandbox per scope"; unrelated scopes only queue when their keys
happen to hash alike. Pinned by `TestE2BPoolProvisionsScopesConcurrently`.

**A rebuild that fails** — hydrate or the `/workspace` probe can fail against a
replacement that was created fine. The executor then destroys the unusable
instance and restores the previous identity, and reports the error to the tool
call. Memory and row agree again, so the next reconcile renews instead of
reading the mismatch as a takeover and adopting the dead sandbox back. The
visible cost when the cause is persistent (a broken workspace store, say) is
one create + destroy per attempt, surfaced as an error each time — not silent
churn.

Alternatives rejected as heavier than the problem:

- **Publish from inside `recreate()` via an output port** (a DTO + callback the
  pool installs). Correct, and it removes the one-call window, but it adds a
  boundary and a second code path for a case the per-use reconcile already
  covers on the very next call.
- **A CAS that also matches the previous `sandbox_id`.** Extra predicate, extra
  state on the row's readers, and it doesn't buy anything here: the `owner`
  check already fences a pod that lost the scope, and a same-pod replay can
  only CAS from the identity it last published.
- **Reading `owner` back from the row to make the distinction.** The column
  exists, but the pool already knows whether *it* rebuilt the sandbox, so no
  schema or record change is needed.

## Files

- `internal/sandbox/lease.go` — port + lease record + default TTL
  (`ReplaceSandboxLease` is the rebuild-publish write)
- `internal/store/sandbox_leases.go` — Postgres/sqlite adapter (DBStore) +
  `EncryptedSandboxLeaseStore` (at-rest token encryption decorator)
- `internal/store/database.go` — the `sandbox_leases` DDL plus
  `migrateSandboxLeasesAddState`, the idempotent retrofit that adds `state` /
  `paused_at` to a table created before them
- `internal/cryptoutil/cipher.go` — neutral at-rest credential cipher
  contract shared with MCP OAuth (`port.Cryptor` aliases it)
- `internal/sandbox/http_error.go` — `sandboxHTTPError`: every provider HTTP
  failure carries its status, so both backends classify "the instance is gone"
  by what the provider said instead of by matching message text (e2b:
  502/404, boxlite: 502/404/410). Covered by
  `TestE2BExecDoesNotRebuildOnNonGoneFailures` and
  `TestIsBoxliteGoneReadsTheStatusNotTheText`.
- `internal/sandbox/e2b_executor.go` — pool adopt/acquire/release integration
- `internal/sandbox/lifecycle.go` — the idle/duty layer above the pool: the
  in-use refcount that keeps a busy scope out of the sweep, `ScopeSleeper`
  (pause instead of destroy), `ScopeExtender` (move both clocks before a long
  operation) and `LeaseRenewer` (renew after one)
- `internal/gateway/userspace.go` — pool wiring: per-pod owner id + lease store
- `docs/sandbox-secret-rotation.md` — key rotation runbook

## Rollout

- **Prerequisite**: `FASTAGENT_OAUTH_SECRET` must be configured. Without it
  the gateway disables shared leases and falls back to per-pod sandboxes —
  plaintext `envd_token` rows are never written.
- No manual migration needed: boot `Migrate()` runs
  `CREATE TABLE IF NOT EXISTS sandbox_leases (...)` (including `epoch`) on
  both dialects. The table ships only with this feature branch — nothing has
  been released, so there is no pre-epoch table to upgrade.
- Verify after rollout: gateway log
  `system sandbox executor pool created backend=e2b ... sharedLeases=true`;
  one session should produce one `e2b sandbox created` even when requests hit
  several pods (later hits log `e2b sandbox adopted from shared lease`). If
  the log shows `sharedLeases=false`, re-check that `FASTAGENT_OAUTH_SECRET`
  is configured before debugging further.

  Counting instances from the log needs the causes separated, which is what the
  lifecycle lines are for: `e2b sandbox provisioned` (a cold scope, with
  `scopeKey` and the total `elapsedMs`) versus `e2b sandbox rebuilt` (a
  replacement, naming `oldSandboxID` → `newSandboxID`) versus
  `e2b sandbox adopted from shared lease` (another pod's instance, no create).
  `e2b sandbox routable` reports how long e2b took to route a fresh id —
  normally the first attempt, and the number to watch when creates are slow.
  The one number to watch over days, not minutes, is how many paused instances
  exist in the account: nothing collects them (see "Orphan reaping" for the two
  queries and the trigger that would bring a reaper back).
- CI coverage: `.github/workflows/go-test.yml` runs the sandbox/store/gateway
  suites against a Postgres service on every push/PR; the live E2B job runs
  only when `E2B_API_KEY`/`E2B_TEMPLATE` secrets exist.

## Fallback ladder (兜底方案)

Degradation is deliberate and ordered — never a hard failure of the sandbox
path:

1. **Full shared leases** — lease store + `FASTAGENT_OAUTH_SECRET`
   configured; one sandbox per scope across replicas.
2. **Registry degraded (runtime errors)** — every read/write failure fails
   open to the local path (see the failure-semantics table): the pool keeps
   or creates its own sandbox, records no epoch when it cannot prove
   ownership, and never destroys anything it may not own. Duplicates are
   transient and cleaned by TTL/timeout.
3. **Encryption key missing** — shared leases are disabled at gateway
   startup (log warning); behavior is identical to the pre-lease per-pod
   pool. Plaintext tokens are never written.
4. **Sandbox feature disabled** (`cfg.Sandbox.Enabled=false`) — no pool;
   file tools fall back to path-only mode, unchanged behavior.

## Fault tolerance (容错方案)

- **Lease store down** (Postgres unreachable, sqlite error): lookup/renew/
  release failures follow the failure table — the local sandbox stays
  usable and nothing shared is destroyed.
- **Pod crash**: its lease expires within TTL (default 15 min) and another
  pod reclaims the scope. The orphaned E2B instance is never destroyed by the
  registry — it becomes one of the residues catalogued in "Orphan reaping".
- **E2B provider failure during create/adopt**: create/hydrate/verify
  failures tear the new sandbox down so callers retry loudly; adopt-renew
  errors keep the adopted executor usable but unregistered; warmup errors
  are best-effort.
- **Race conditions**: concurrent acquire yields exactly one winner; losers
  adopt the winner; the double race (Acquire loss + CAS adoption miss) keeps
  the local unregistered sandbox until the next reconcile. This covers racing
  *acquirers* only — concurrent **rebuilds** inside one executor are not
  synchronised; see the open violators in the invariant table.
- **Registry state corruption** (undecryptable or wrong-key row): reads fail
  closed → local create; the stale row is reclaimed at expiry and can never
  cause a destroy.

## Compatibility & upgrade considerations

- **Schema**: new table only (`CREATE TABLE IF NOT EXISTS`), identical on
  sqlite/Postgres; no existing table changes, so single-node and multi-pod
  installs share one code path.
- **Behavior default**: without the secret (or without a lease store) shared
  leases are off — existing deployments observe no change.
- **API compatibility**: `SandboxLeaseStore` is additive; the gateway pool
  wiring gains optional lease/owner inputs and old call sites keep working
  with the per-pod path.
- **Rolling upgrade**: replicas without the secret run per-pod while
  replicas with it share leases; both are safe (no plaintext, no
  cross-destroy), but the mixed window may briefly create duplicate
  sandboxes for the same scope. Completing the rollout converges them.
- **Key rotation**: rows written under the old key become unreadable
  (fail-open local create) and are reclaimed at TTL; rotation never destroys
  sandboxes but orphans instances until the provider timeout — see
  [sandbox-secret-rotation.md](./sandbox-secret-rotation.md).
- **Logging**: new log fields are additive; tokens are never logged.

## Security note (v1)

`envd_token` is encrypted at rest with AES-256-GCM, using the same master
secret as MCP OAuth tokens (`FASTAGENT_OAUTH_SECRET`, derived to a 32-byte
key via SHA-256; see `internal/mcp/oauth/adapter/cryptor.go`). The ciphertext
is **base64-encoded into a universal `TEXT` column** — no `BLOB`/`BYTEA`, no
dialect branching, identical schema on sqlite and Postgres (the same
dialect-neutral approach used elsewhere in this repository). Encryption is
applied by `store.EncryptedSandboxLeaseStore`, assembled at the gateway
composition root; `DBStore` itself stays key-agnostic.

- Shared leases require `FASTAGENT_OAUTH_SECRET` to be set. Without it the
  gateway logs a warning and keeps per-pod sandboxes — plaintext `envd_token`
  rows are never written.
- Rotating the secret makes existing rows undecryptable. Reads fail closed at
  the wrapper, the pool treats it as a registry lookup error and falls back
  to a local create (fail-open), and the stale row is reclaimed on expiry or
  takeover. Rotation is therefore safe but leaves orphaned sandboxes until
  their E2B timeout — follow
  [sandbox-secret-rotation.md](./sandbox-secret-rotation.md).

Threat model and controls:

- **DB dump / backup leak** → AES-256-GCM at rest; the key is never in the
  DB, ConfigMap, or logs.
- **Cross-pod impersonation** → `sandbox_id` + `envd_token` grant access to
  one short-lived E2B instance only; the account-level API key never touches
  the database.
- **Stale/delayed destroy** → destruction requires `owner + epoch`; any
  stale release fails closed and leaves the sandbox alive.
- **Wrong-key reads after rotation** → decryption failure is treated as a
  registry read error (fail-open local create); ciphertext is never returned
  as if it were a token.

## Known tradeoffs (accepted)

- Whenever a lease lapses — the pod crashed, or it simply went idle past TTL
  (default 15 min) — another pod may take the scope. If it takes over **before**
  the row is overwritten it adopts the same `sandbox_id`, and with `autoPause`
  that instance is merely paused: the takeover resumes it instead of building a
  replacement. A takeover that wins `Acquire` *after* the row was replaced
  leaves the old instance paused, unreferenced and permanent — unbilled and
  outside the concurrency limit, so the cost is bookkeeping rather than money.
  Nothing collects it today; "Orphan reaping" above is the full account, the
  queries that measure it, and the trigger that would change that.
- Adoption races are benign for correctness of destruction (owner check), but
  two pods briefly sharing one sandbox is expected during takeover windows.
- In the rare double-race where a creator loses `Acquire` and the subsequent
  CAS adoption also misses, the pool keeps its own unregistered sandbox until
  the next reconcile; because no epoch was recorded, its later release will
  not destroy it and it lives until the e2b timeout. Logged as a warning;
  accepted for v1.
- A rebuild is published on the **next** `Get`, not from inside `recreate()`.
  The window is one call wide and only matters to a sibling replica that hits
  the same scope inside it: that replica adopts the dead instance, pays one
  rebuild of its own, and converges on the same identity. Publishing
  immediately would remove the window at the cost of a second code path; the
  reconcile already runs before every use, so v1 accepts the window.
- The `rebuilt` flag is in-memory, so a pod that rebuilds and then crashes
  before its next `Get` leaves the row on the dead sandbox. That is the
  pre-existing crash story (the row lapses at TTL and the next acquirer
  replaces it), unchanged by this stage.
- **Lock granularity** — `p.mu` guards only the executor/epoch maps; the
  provisioning path runs under a per-scope striped lock (see Stage 2b →
  Concurrency). Two consequences are accepted rather than solved: unrelated
  scopes that hash to the same stripe queue behind each other, and `CloseAll`
  does not take the scope locks, so a `Get` racing shutdown can register an
  executor after the drain — the pre-existing "in-flight work dies with the
  process" behavior, not a new hazard.
- **Waiting for a fresh sandbox costs latency when the provider is unwell** —
  `waitUntilRoutable` polls for up to 60s before giving up. That bound is only
  ever paid when e2b accepts a create and then cannot route the instance, in
  which case the old behavior failed at hydrate with a `502 not found` that
  looked like a dead sandbox. A sandbox the provider reaps outright (account
  limits, billing, an unwell region) still fails — this retry buys routing
  time, not instances.
- **Only long operations move the expiry.** `extendThreshold` (60s) keeps the
  common tool call from paying an API round trip, which leaves a narrow window:
  an operation shorter than 60s that starts seconds before the expiry can still
  be paused mid-flight. The stream is cut, the process survives in the snapshot,
  and the caller sees a truncated response it can retry — accepted, because
  closing that window means a call per tool call.
- **A long operation holds the scope longer than the TTL would.** Renewing the
  lease for the operation's budget means the row stays this pod's until the
  operation ends — which is the point, but it also delays a takeover that would
  otherwise have happened at TTL. The alternative (letting the row lapse while
  the operation runs) trades a slow takeover for a second sandbox, which the
  lease exists to prevent.
- **Two things about the secure switch are not verifiable from here.**
  [UNVERIFIED] the mixed fleet during a rolling deploy: sandboxes created before
  `secure: true` carry no token, and envd answers them without auth, so calls to
  them still work (our client simply sends no `X-Access-Token`). Worth one check
  against a real account. (The other half of this bullet is settled: `DELETE`
  does apply to a paused sandbox — "A paused sandbox is only removed when you
  explicitly call `kill` on it",
  [paused sandboxes and concurrency](https://docs.e2b.dev/faq/paused-sandboxes-concurrency)
  — which is why a release of a scope whose sandbox is asleep does not leak it.)
- **The in-use marker covers the post-exec sync too.** The sync reads the
  sandbox, so the scope stays marked busy until it finishes; otherwise the sweep
  could pause the sandbox mid-sync. That also means a wedged sync holds the scope
  out of idle handling for its duration — the same trade as a long exec, and the
  same reason the sweep prefers its own clock over a lease on the work.

## Test topology

**Last reviewed**: 2026-09-09

Test layers mirror the production dependency direction (policy → port →
adapter → composition root), so each seam is exercised without pulling
outer layers inward:

Why this shape: policy tests must never need a database or network, adapter
tests must exercise real SQL (not mocks), composition-root tests only assert
wiring decisions, and the live e2e is the single place that pays for real
external dependencies.

- **Policy (sandbox package)** — `lease_pool_test.go` drives the pool with a
  `fakeLeaseStore` that implements `SandboxLeaseStore` and can inject errors
  per operation. Covers every fail-open path: lookup/acquire/adopt-renew/
  release/CloseAll failures keep sandboxes alive; reconcile registry errors
  keep the local executor; the double race (Acquire loss + CAS adoption miss)
  keeps the local sandbox unregistered. No database involved.
- **Policy, rebuild (sandbox package)** — `lease_rebuild_test.go` shares that
  fake and drives the whole rebuild path offline: envd is stubbed at the HTTP
  boundary (the outermost layer) and the create call is injected, so
  hydrate → probe → mark → republish runs for real. Covers the republish
  invariant, the registry-error retry, the superseded-by-another-pod CAS miss,
  the expiry re-acquire route (which publishes by construction), and the
  no-lease-store no-op. It is also where the executor's own concurrency is
  pinned: parallel rebuilds fan in to one replacement, and a rebuild that
  cannot hydrate restores the previous identity and destroys the replacement.
  `e2b_error_classification_test.go` covers the two decisions that used to be
  guesses: only a 502/404 status from envd counts as "gone" (a message that
  merely mentions those codes does not), and the destroy answer is read —
  2xx/404 succeed, anything else is an error naming the sandbox.
  `e2b_readiness_test.go` covers the gap between `POST /sandboxes` returning an
  id and the edge being able to route it: ready on the first try, retried until
  routable, given up on with the sandbox and the bound named, and never retried
  for a verdict a rebuild cannot fix.
  `e2b_hydrate_retry_test.go` covers the other half of that window — a stream a
  fresh container cut while booting — and the diagnostics that make such a
  failure actionable: the Connect **trailer** is parsed instead of discarded
  (it is where the protocol carries a server-side error) and the frames that
  did arrive are named in the error, so "truncated" is never the whole story.
  `e2b_create_body_test.go` pins the wire contract behind the lifecycle
  redesign: the create body must ask for pause-on-timeout with a memory
  snapshot and auto-resume, because losing those fields silently puts the
  rebuild path back on the happy path.
  `lifecycle_sleep_test.go` covers the idle sweep's two hazards: an operation
  that outlives `idleTTL` must not have its sandbox reclaimed underneath it,
  and a sandbox that cannot be slept must survive (only "nothing to sleep"
  falls through to release).
  `e2b_token_probe_test.go` is credential-gated (`-tags=manual`): it creates a
  sandbox both ways, checks the schema's claim that only `secure: true` yields
  an `envdAccessToken`, then pauses and resumes a secure one to see whether the
  create-time token still authenticates or has to be replaced by the one
  `connect` returns.
  `e2b_timeout_token_test.go` covers the two repairs a long operation needs:
  moving the instance expiry before a long op (and not paying for it on a short
  one), and recovering from a superseded envd token by reconnecting rather than
  rebuilding.
  `lease_state_test.go` (sandbox) and `sandbox_leases_state_test.go` (store)
  cover the running/paused marker end to end: the sweep writes it, adoption
  reads it and resumes, a running row skips the round trip, renewing does not
  reset it, a rebuild does, a foreign owner cannot write it, and the retrofit
  adds the columns exactly once to a table that predates them.
  The store's state tests also exist in their Postgres form
  (`TestSandboxLeaseStatePostgres`, `TestSandboxLeaseStateMigrationPostgres`),
  because production is Postgres and neither the retrofit's information_schema
  lookup nor its ALTER is exercised by sqlite.
- **Adapter (store package)** — `sandbox_leases_test.go` runs `DBStore`
  through `sandbox.SandboxLeaseStore` against real sqlite: renew CAS miss,
  stale/missing release no-ops, monotonic epoch. `sandbox_leases_postgres_test.go`
  `sandbox_leases_replace_test.go` pins the rebuild write against real sqlite:
  a foreign owner cannot move the row, the owner's replace bumps the epoch and
  rewrites sandbox/token/template, a stale-epoch destroy after it fails
  closed, and an expired row is not replaceable. `sandbox_leases_postgres_test.go`
  covers the production dialect (concurrent acquire single winner, stale
  release fencing, idempotent `Migrate`) and is gated by
  `FASTAGENT_TEST_PG_DSN` — sqlite serializes writes, so cross-connection
  semantics are only proven on Postgres. `sandbox_leases_crypto_test.go`
  covers the encryption decorator: round-trip with no plaintext in the raw
  row, wrong-key fail-closed, and rotation (old-key rows unreadable → new
  key reclaims after TTL) on sqlite, with a Postgres variant gated by
  `FASTAGENT_TEST_PG_DSN`.
- **Composition root (gateway package)** — `sandbox_pool_lease_test.go` tests
  the pure `sandboxLeaseOpts` decision (nil store / missing owner ⇒ no shared
  lease) and `buildSystemSandboxPool` wiring without network access.
- **Live e2e (`TestE2BPoolCrossPodAdoption`)** — requires `E2B_API_KEY` and
  `E2B_TEMPLATE`; in CI a missing credential set fails the test instead of
  silently skipping. Each "pod" opens its own `DBStore` handle over the same
  underlying database (Postgres when `FASTAGENT_TEST_PG_DSN` is set,
  otherwise two sqlite connections to one `?cache=shared` file). Stages:
  1. Pod A creates a sandbox, registers the lease, and writes a marker into
     `/workspace`.
  2. Pod B adopts the same `sandbox_id` (epoch bumped) and must read the
     marker back — proving it runs on the same E2B instance, not a second
     sandbox with a copied lease row.
  3. Pod A releases with its stale epoch: the lease must survive and Pod B
     must still execute and still read the marker.
- **Live e2e, rebuild (`TestE2BPoolCrossPodRebuild`)** — same gating and same
  two-handle setup. Stages:
  1. Pod A creates the sandbox and the lease records it.
  2. The instance is destroyed out of band while the pool still holds the
     handle — the `502 sandbox not found` signal a provider-side timeout also
     produces — and the next exec must rebuild transparently.
  3. The next `Get` republishes: the row must name a *different* sandbox than
     the destroyed one.
  4. Serving again must keep that identity (no new sandbox per call — the
     churn this stage removes).
  5. Pod B adopts the rebuilt instance and reads a marker A wrote *after* the
     rebuild, proving both replicas are on the replacement rather than on a
     duplicate or on the corpse.
- **CI** — `.github/workflows/go-test.yml` runs the sandbox/store/gateway
  suites against a Postgres service **with `-race`** (the sweeper goroutine and
  the parallel-rebuild tests make races the interesting failure, and the test
  doubles needed locks of their own the first time it ran); a second job runs
  the live E2B e2e only when `E2B_API_KEY`/`E2B_TEMPLATE` secrets exist.
- **Fallback / compatibility** — `TestSandboxLeaseStoreFrom` asserts shared
  leases are disabled without `FASTAGENT_OAUTH_SECRET` (never plaintext);
  `TestBuildSystemSandboxPoolWiring` covers disabled config → nil pool and
  e2b → pool; `TestE2BPoolCloseAllHonorsLeaseStore` covers nil-store
  per-pod close-all; `TestSandboxLeaseMigrateIdempotentPostgres` proves
  double `Migrate()` on Postgres is a no-op.

### How to run

```bash
# Unit + adapter + gateway (no external deps)
go test ./internal/sandbox/ ./internal/store/ ./internal/gateway/ -count=1

# Same, with the race detector — this is what CI runs, and it is the only way
# the pool's background sweeper and the parallel-rebuild path get checked.
go test ./internal/sandbox/ ./internal/store/ ./internal/gateway/ -count=1 -race

# Rebuild publish only (offline): policy + adapter
go test ./internal/sandbox/ ./internal/store/ -run 'Rebuil|ReplaceSandbox' -count=1 -v

# Postgres semantics (start any local PG first)
FASTAGENT_TEST_PG_DSN='postgres://postgres@localhost:5432/postgres?sslmode=disable' \
  go test ./internal/store/ -run Postgres -count=1 -v

# Encryption decorator + rotation on Postgres (same env gate)
FASTAGENT_TEST_PG_DSN='postgres://postgres@localhost:5432/postgres?sslmode=disable' \
  go test ./internal/store/ -run '^TestEncryptedSandboxLeaseStorePostgres$' -count=1 -v

# Live E2B cross-pod adoption (requires credentials)
E2B_API_KEY='...' E2B_TEMPLATE='...' \
  go test ./internal/sandbox/ -run '^TestE2BPoolCrossPodAdoption$' -count=1 -v

# Live E2B cross-pod rebuild (requires credentials)
E2B_API_KEY='...' E2B_TEMPLATE='...' \
  go test ./internal/sandbox/ -run '^TestE2BPoolCrossPodRebuild$' -count=1 -v
```
