# Sandbox lease secret rotation runbook

`sandbox_leases.envd_token` is encrypted at rest with AES-256-GCM keyed by
`FASTAGENT_OAUTH_SECRET` (the same master secret that protects MCP OAuth
refresh tokens). This runbook covers rotating that secret.

## Impact

- **Existing `sandbox_leases` rows become unreadable** immediately after the
  secret changes on all replicas: reads fail closed at
  `store.EncryptedSandboxLeaseStore`, the pool treats it as a registry
  lookup error and falls back to a local create (fail-open). No plaintext is
  ever written, and no sandbox is destroyed by the rotation itself.
- **Stale rows are reclaimed by TTL**: the next `AcquireSandboxLease` can
  only replace an expired row (default TTL 15 min), so the old rows are
  replaced within TTL or removed on release.
- **A rebuild re-keys its own row**: when a replica replaces a sandbox whose
  row it still owns, `ReplaceSandboxLease` rewrites `envd_token` under the
  current secret. Such rows become readable again without waiting for TTL;
  the rest still age out as above.
- **Orphaned E2B instances** created before rotation live until their
  provider timeout (~30 min). They are not destroyed by this runbook.
- **MCP OAuth is affected too**: the same secret encrypts stored OAuth
  refresh tokens. Rotating it invalidates every stored refresh token and
  forces all users/agents to re-authorize. Coordinate the window and notify
  owners before rotating.

## Steps

1. Generate a new secret:

   ```bash
   openssl rand -hex 32
   ```

2. Update the secret on every replica (helm value `oauth.secret` →
   `FASTAGENT_OAUTH_SECRET`; stored in the fastagent secret, never in a
   ConfigMap). Roll out so all replicas converge on the same value.
3. Optional fast cleanup (do NOT destroy sandboxes directly): delete the
   stale lease rows so the next request creates fresh leases immediately
   instead of waiting for TTL:

   ```sql
   DELETE FROM sandbox_leases;
   ```

   Orphaned E2B instances then expire on their own provider timeout.
4. Verify:

   ```bash
   # gateway log shows shared leases active
   grep 'system sandbox executor pool created' <gateway-log> | grep sharedLeases=true
   # a new session should log 'e2b sandbox created' (fresh lease) and later
   # 'e2b sandbox adopted from shared lease' on sibling pods
   ```

5. Confirm the table holds no plaintext tokens:

   ```sql
   SELECT envd_token FROM sandbox_leases LIMIT 1;
   -- value must be base64 ciphertext, not the raw E2B token
   ```

## Regression coverage

- `TestEncryptedSandboxLeaseStoreRotation` (sqlite): old-key row unreadable,
  new key reclaims after TTL expiry.
- `TestEncryptedSandboxLeaseStoreReplaceSandbox` (sqlite): the rebuild write
  encrypts the replacement token — the raw row holds ciphertext, and the
  decorator decrypts it back.
- `TestEncryptedSandboxLeaseStorePostgres` (Postgres, gated by
  `FASTAGENT_TEST_PG_DSN`): same semantics on the production dialect plus a
  raw-row plaintext check.
