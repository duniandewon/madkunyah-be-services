# Current Feature

<!-- Feature name and short description -->

**Authentication & Authorization** — passwordless OTP login (WhatsApp/email) for customers and staff, JWT access + rotating refresh tokens, POS device enrollment with local PIN unlock and supervisor step-up, and role-based, outlet-scoped authorization. Full spec: `context/features/auth-spec.md`.

## Status

<!-- Not started | In Progress | Completed -->

Completed — **Slice 1** (OTP login + JWT/refresh sessions). The remaining goals below (authz enforcement, staff PIN, POS devices, step-up, customer identifiers) carry over to the next slices; see "Deferred" under Notes.

## Goals

<!-- Goals and requirements -->

### Identifiers (§3.1)

- Accept phone or email; normalize phones to E.164 (`08…`/`628…`/`+62 8…` → `+628…`), lowercase/trim emails
- Malformed input → `400 invalid_identifier`

### OTP request — `POST /v1/auth/otp/request` (§3.2)

- 6-digit CSPRNG code; store only `HMAC-SHA256(otp_secret, purpose ‖ identifier ‖ code)` in Redis, TTL 5m
- New request replaces the existing code and resets attempts
- Cooldown 60s per identifier (all purposes); quota 5/identifier/day (Asia/Jakarta), 20/IP/hour — checked **before** existence lookup
- Staff purposes (`staff_login`, `device_enroll`): send only to active staff with a role (and `device.enroll` for enrollment); otherwise store a decoy record
- Customer purpose: always send (unknown = signup)
- Async dispatch via job queue (WhatsApp for phone, SMTP for email, email fallback for known accounts); failed dispatch refunds quota slot
- Always `202 { status: "sent_if_eligible", expires_in: 300, resend_after: 60 }`
- Log `otp.requested` to `auth_events`

### OTP verify — `POST /v1/auth/otp/verify` (§3.3)

- Constant-time HMAC compare; atomic attempt counter, record deleted on 5th wrong code
- Success: atomic GET-and-DELETE (Lua) — exactly one concurrent winner
- Every failure → `401 otp_invalid`; verify rate limit 30/IP/hour
- `customer_login`: create/verify customer, link guest orders by phone
- `staff_login`: issue staff session; `device_enroll`: proceed to enrollment
- Log `otp.verified` / `otp.failed`

### Tokens & sessions (§3.4)

- ES256 access tokens with `kid`, 15m, identity claims only (`iss`, `aud`, `sub`, `jti`, `iat`, `exp`, `fam`; `pos` adds `outlet_id`, `device_id`)
- Opaque 256-bit refresh tokens, SHA-256 hashed in Redis, tracked per family
- Lifetimes: customer 30d absolute, staff 12h absolute, POS 90d sliding
- Rotation on refresh; reuse of a rotated token revokes the family (`401 session_revoked`), except a 30s retry grace window returning the same successor pair
- Logout (`POST /v1/auth/logout`), revoke-all (`POST /v1/auth/sessions/revoke-all`)
- JWKS at `GET /.well-known/jwks.json`, retired keys kept ≥ 30 min
- One audience per route group; wrong `aud` → `401 token_invalid`

### Staff PIN (§3.5)

- 6 digits, reject weak PINs (repeats, runs, denylist), stored as argon2id
- Set via `POST /v1/staff/me/pin` (staff session only); reset via `DELETE /v1/staff/{id}/pin` (`staff.manage`)
- Not unique, no lockout, local unlock only — `pin.failed` synced for reporting

### POS devices (§3.6)

- `POST /v1/auth/devices/enroll` after `device_enroll` OTP; creates `devices` row + `pos` refresh family
- One active device per outlet (configurable); `409 device_limit_reached` unless `replace: true`
- `GET /v1/pos/roster` — staff IDs, names, PIN hashes, outlet permissions, roster version
- `POST /v1/auth/devices/{id}/revoke` → `401 device_revoked` on next refresh/sync
- POS calls carry `X-Acting-Staff-Id`; update `devices.last_seen_at` on refresh/sync

### Supervisor step-up (§3.7)

- Required for `order.void.paid`, `order.discount`, `order.price_override`, `shift.open`, `shift.close` when actor lacks permission
- Record both `acting_staff_id` and `approved_by`; one approval per action
- On sync, re-check approver's permission at action time; accept but flag `authz.offline_violation` on failure

### Authorization (§3.8)

- Fixed v1 permission matrix for cashier / supervisor / manager / owner
- Outlet-scoped checks; permissions resolved per request, cached at `authz:staff:{id}`, invalidated on change
- No role at outlet → `404 not_found`; role but no permission → `403 forbidden`
- Deactivation revokes all families and invalidates cache
- No privilege escalation (managers grant only cashier/supervisor; no self-edits)
- Refund separation of duties (owners exempt); global roles via `staff_global_roles`
- Customer tokens limited to own orders/profile; public `GET /v1/track/{tracking_token}`

### Customer accounts (§3.9)

- Guest order linking on phone verify
- `POST /v1/me/identifiers` to add/change identifier via OTP; `409 identifier_in_use`, no auto-merge

### Audit (§3.10)

- `auth_events` and `audit_logs` events as listed in spec; both append-only (no UPDATE/DELETE grant)

### Schema changes (§8)

- `customers.phone` nullable + identifier CHECK; `staff_global_roles` table; `audit_logs.approved_by`; `devices.revoked_at` / `revoked_by`; `staff.deactivated_at`; role-grant history
- New Redis keys: `otp:verify:ip:{ip}`, `refresh:successor:{old_hash}`, `authz:staff:{staff_id}`

## Notes

<!-- Any extra notes -->

- Unified error body: `{ "error": { "code", "message", "retry_after?" } }` — full code table in spec §5.1
- Redis down → OTP/refresh/enroll fail closed (`503 auth_unavailable`); `revoked:{jti}` check fails open; authz cache falls back to Postgres
- Logs never contain codes, tokens, PINs, or unmasked identifiers
- Browser refresh tokens go in `HttpOnly; Secure; SameSite=Strict` cookie scoped to `/v1/auth`
- All limits/TTLs/lifetimes are config, not code; notifier and clock must be injectable for tests
- Three independent secrets: OTP HMAC secret, JWT signing keys, refresh-hash pepper
- Open questions OQ-1 … OQ-8 (spec §10) use their working assumptions unless changed
- Acceptance criteria AC-1 … AC-33 (spec §6) define the integration test suite
- This is large — implement in slices (identifiers + OTP → tokens → authz → devices/PIN → customer extras), each on its own branch/commit

### Slice 1 — `feature/auth-otp-sessions` (completed)

- Done: migration `0001_auth` (outlets, staff, devices, roles + v1 matrix seed, staff_outlet_roles, staff_global_roles, customers, auth_events); identifier parsing; OTP request/verify with cooldown, quotas, decoys, attempt limit, verify IP limit; ES256 tokens + JWKS (with optional `JWT_PREVIOUS_PUBLIC_KEY`); refresh rotation, reuse detection, grace window; logout; revoke-all; audience middleware; `GET /v1/me`, `GET /v1/staff/me`
- OTP delivery: `Notifier` interface with a log stub (code printed only when `APP_ENV=development`), in-process dispatcher with retry, email fallback and quota refund
- Deviations: `device_enroll` purpose not accepted yet (lands with devices slice); `revoke-all` accepts customer **or** staff tokens since it only acts on the caller's own sessions; `auth_events.identifier` stores the full identifier (NFR-SEC-5) rather than masked (FR-OTP-14); logout revokes the whole family, not just the presented token; extra Redis key `refresh:subject:{type}:{id}` indexes families for revoke-all
- Deferred to later slices: guest-order linking (needs `orders`), authz enforcement/cache, devices/PIN/roster/step-up, `/v1/me/identifiers`, admin revoke for other staff, append-only DB grants (FR-AUD-3), metrics, testcontainers integration suite
- Browser cookies need `Access-Control-Allow-Credentials` + explicit origins once the shop/dashboard run on another origin

## History

<!-- Keep this updated from earliest to latest -->

- **2026-10-13** — Initial setup: HTTP skeleton (chi), typed env config, Postgres + Redis connections, `/health` and `/ready` endpoints.
- **2026-10-13** — Added project context docs (`CLAUDE.md`, `context/`): project spec, coding standards, AI interaction guidelines, and this feature tracker.
- **2026-10-16** — Auth slice 1 (`feature/auth-otp-sessions`): migration `0001_auth` with role seed, phone/email identifiers, OTP request/verify with cooldown, quotas, decoys and attempt limits, ES256 access tokens + JWKS, rotating refresh sessions with reuse detection and grace window, logout, revoke-all, audience middleware, log-stub notifier. Dev DB reset to this repo's migrations; coding standards updated to pgx; golangci-lint clean.
