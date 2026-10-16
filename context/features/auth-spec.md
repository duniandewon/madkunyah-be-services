# Authentication & Authorization — Feature Spec

## 1. Introduction

This spec defines how every person and device proves who it is to the F&B backend, and what each of them is allowed to do once it has.

The system is **passwordless**. Customers and staff authenticate with a one-time code (OTP) sent over WhatsApp or email. A successful verification issues a short-lived JWT access token and a rotating refresh token. The POS is offline-first, so it gets special handling. A manager enrolls the POS device once using OTP. After that, cashiers unlock the trusted device with a local 6-digit PIN that works without connectivity. Supervisors approve sensitive actions on the same device by entering their own PIN.

Authorization is **role-based and outlet-scoped**. One person can be a cashier at Outlet A and a manager at Outlet B. Every staff action is attributed to a person and, where relevant, to an approver.

### 1.1 Goals

- No passwords stored anywhere, for anyone.
- A cashier can always open the till and sell, even with no internet.
- A stolen token, lost device, or departed employee can be cut off server-side within minutes.
- OTP spend on the WhatsApp API is bounded and abuse-resistant.
- Every authentication event and every privileged action leaves a durable, attributable trail.

### 1.2 In scope

- OTP request and verification for customers and staff
- JWT access tokens, refresh-token rotation, reuse detection, logout, revocation
- POS device enrollment, the device session, revocation, and offline behaviour
- Staff PIN: setting, resetting, local unlock, supervisor step-up approval
- Roles, permissions, outlet scoping, and enforcement middleware
- Customer self-signup on first verify, guest-order linking, adding a second identifier
- Auth-related rate limiting, auditing, and error contract

### 1.3 Out of scope (v1)

- Passwords, social login (Google/Apple), SSO, TOTP/authenticator apps
- Custom roles created by admins (the four roles are fixed; permissions are data so this can come later)
- Customer account deletion and data export (tracked separately under data-protection work)
- Automatic merging of two existing customer accounts
- Kitchen display authentication (KDS is out of v1)

### 1.4 Decisions confirmed for this spec

| Topic | Decision |
| --- | --- |
| Supervisor approvals on POS | Supervisor enters **their own PIN** on the cashier's device (step-up). Works offline. |
| OTP quotas | **5 per identifier per day**, **20 per IP per hour** (configurable) |
| PIN | **6 digits**. **No lockout** after wrong attempts. Failures are recorded, not blocked. |
| Customer identifier | **Phone or email**. Requires a schema change (§8). |

---

## 2. Actors and Credentials

| Actor | Client | Primary credential | Session | Token audience |
| --- | --- | --- | --- | --- |
| Guest customer | Online shop | None (`tracking_token` for order lookup only) | — | — |
| Customer (account) | Online shop | OTP to phone or email | 30-day refresh, absolute | `customer` |
| Staff on dashboard | Dashboard | OTP to phone or email | 12-hour refresh, absolute | `staff` |
| POS device | POS | Enrolled once via a manager's OTP | 90-day refresh, sliding | `pos` |
| Staff on POS | POS | Local 6-digit PIN (device unlock only) | Local session on enrolled device | Acts under the device token |
| System | Workers, webhooks | Service credentials / provider signatures | — | — |

**Note on the `pos` audience:** the overview describes two audiences, staff and customer. This spec adds a third, `pos`, because a device session has a different subject (a device, not a person) and a different trust model. See Open Question OQ-1.

---

## 3. Functional Requirements

Requirement IDs are stable and referenced by the acceptance criteria in §6.

### 3.1 Identifier handling

- **FR-ID-1** Accept an identifier that is either a phone number or an email address. Detect the type from the format.
- **FR-ID-2** Normalize phone numbers to E.164 before any lookup, key construction, or storage. Indonesian local formats (`08…`, `628…`, `+62 8…`) all become `+628…`.
- **FR-ID-3** Normalize emails by trimming whitespace and lowercasing. Do not strip `+tags` or dots.
- **FR-ID-4** Reject malformed identifiers with `400 invalid_identifier`. This is the only OTP-request error that depends on input content, and it reveals nothing about existence.

### 3.2 OTP request — `POST /v1/auth/otp/request`

Request body: `{ identifier, purpose }`, where `purpose ∈ customer_login | staff_login | device_enroll`.

- **FR-OTP-1** Generate a 6-digit code using a CSPRNG, uniformly distributed, with leading zeros allowed.
- **FR-OTP-2** Store `HMAC-SHA256(otp_secret, purpose ‖ identifier ‖ code)` in Redis at `otp:{purpose}:{identifier}`, with `attempts = 0`, `sent_at`, and a TTL of 5 minutes. Never store the raw code.
- **FR-OTP-3** A new request for the same `purpose` and identifier **replaces** any existing code. The old code stops working immediately and the attempt counter resets.
- **FR-OTP-4** **Cooldown:** reject a request within 60 seconds of the previous one for the same identifier, across all purposes. Return `429 otp_cooldown` with `Retry-After`.
- **FR-OTP-5** **Identifier quota:** at most 5 sends per identifier per calendar day in Asia/Jakarta, across all purposes. Return `429 otp_quota_exceeded` when exceeded.
- **FR-OTP-6** **IP quota:** at most 20 requests per source IP per rolling hour. Return `429 otp_quota_exceeded` when exceeded.
- **FR-OTP-7** Apply cooldown and quota checks **before** the existence lookup. They then behave identically whether or not the identifier exists.
- **FR-OTP-8** **Staff scope:** for `staff_login` and `device_enroll`, send a code only if the identifier belongs to an active staff member who holds at least one role. For `device_enroll`, that role must also carry `device.enroll` at the target outlet. Otherwise send nothing, but store a **decoy** OTP record so the verify step behaves identically (see FR-VER-6).
- **FR-OTP-9** **Customer scope:** for `customer_login`, always send. An unknown identifier is a signup.
- **FR-OTP-10** **Delivery channel:**
  - A phone identifier is sent over WhatsApp.
  - An email identifier is sent over SMTP.
  - If WhatsApp dispatch fails permanently and the matched account has an email on file, fall back to email. Unknown identifiers have no fallback.
- **FR-OTP-11** Dispatch asynchronously through the job queue. The HTTP response must not wait on, or reveal, delivery outcome.
- **FR-OTP-12** Always respond `202 Accepted` with the same body and headers for every non-rate-limited, well-formed request: `{ "status": "sent_if_eligible", "expires_in": 300, "resend_after": 60 }`.
- **FR-OTP-13** A dispatch that fails permanently does not count against the identifier's daily quota. The cooldown still applies.
- **FR-OTP-14** Write an `auth_events` row `otp.requested` with `subject_id` (null if unknown), the masked identifier, IP, and user agent.

### 3.3 OTP verify — `POST /v1/auth/otp/verify`

Request body: `{ identifier, purpose, code, device? }`.

- **FR-VER-1** Compare HMACs in constant time.
- **FR-VER-2** Increment `attempts` atomically on every verify call. On the 5th wrong code, delete the OTP record. Further attempts fail until a new code is requested.
- **FR-VER-3** On success, delete the OTP record atomically (GET-and-DELETE in one Lua script). Of two concurrent correct submissions, exactly one succeeds.
- **FR-VER-4** Return a single error, `401 otp_invalid` ("Code is invalid or has expired. Request a new code."), for every failure: wrong code, expired, no code issued, attempts exhausted, or decoy record.
- **FR-VER-5** Rate-limit verify calls per IP: 30 per hour by default, configurable. This bounds brute-force attempts spread across many identifiers.
- **FR-VER-6** Decoy records for unknown staff identifiers carry a random HMAC. They count attempts and expire exactly like real records, so they can never verify.
- **FR-VER-7** On `customer_login` success:
  - If no customer has this identifier, create one and set `verified_at`.
  - If one exists and `verified_at` is null, set it.
  - If the identifier is a phone, link guest orders (FR-CUS-1).
- **FR-VER-8** On `staff_login` success, issue a `staff` session (§3.4).
- **FR-VER-9** On `device_enroll` success, proceed to device enrollment (§3.6). The verify response does not itself issue a `staff` session.
- **FR-VER-10** Write `otp.verified` or `otp.failed` to `auth_events`.

### 3.4 Tokens and sessions

- **FR-TOK-1** Sign access tokens with **ES256** and include a `kid` header. Lifetime is 15 minutes.
  - Claims: `iss`, `aud` (`customer` | `staff` | `pos`), `sub`, `jti`, `iat`, `exp`, `fam` (family ID).
  - `pos` tokens also carry `outlet_id` and `device_id`.
- **FR-TOK-2** Access tokens carry **identity only, not roles or permissions.** Permissions are resolved per request (FR-AZ-3), so a role change takes effect without waiting for token expiry.
- **FR-TOK-3** Refresh tokens are opaque, 256-bit random values. Store only their SHA-256 hash in Redis at `refresh:{hash}`, holding `{subject_id, subject_type, device_id, family_id, issued_at, family_started_at}`. Each family is tracked in `refresh:family:{family_id}`.
- **FR-TOK-4** Refresh lifetimes:

  | Session | Lifetime | Behaviour |
  | --- | --- | --- |
  | `customer` | 30 days | Absolute from `family_started_at`; rotation does not extend it |
  | `staff` (dashboard) | 12 hours | Absolute; staff re-OTP each working day |
  | `pos` | 90 days | **Sliding**; each successful refresh resets the 90-day window |

- **FR-TOK-5** **Rotation** (`POST /v1/auth/token/refresh`): every successful refresh invalidates the presented token and issues a new access + refresh pair in the same family.
- **FR-TOK-6** **Reuse detection:** if a token that has already been rotated is presented, revoke the **entire family**. Return `401 session_revoked` and write `token.reuse_detected`. This rule has one exception, FR-TOK-7.
- **FR-TOK-7** **Retry grace window:** if a rotated token is re-presented within **30 seconds** of its rotation, from the same `device_id` (POS) or the same client fingerprint, return the *same* successor pair instead of revoking. The successor is stored at `refresh:successor:{old_hash}` with a 30-second TTL. This covers lost responses on flaky networks, which are routine for the POS.
- **FR-TOK-8** **Logout** (`POST /v1/auth/logout`): delete the current refresh token and add the current access token's `jti` to `revoked:{jti}` until its expiry.
- **FR-TOK-9** **Revoke all sessions** (`POST /v1/auth/sessions/revoke-all`): revoke every family for the caller. Admins can do the same for any staff member.
- **FR-TOK-10** Publish verification keys at `GET /.well-known/jwks.json`. During key rotation, keep a retired key published for at least 30 minutes, which is twice the access-token lifetime.
- **FR-TOK-11** Use separate middleware per audience. A route group accepts exactly one audience. A token with the wrong `aud` is rejected as `401 token_invalid`, never as `403`.

### 3.5 Staff PIN

- **FR-PIN-1** PINs are exactly 6 digits.
- **FR-PIN-2** Reject trivially weak PINs: all one digit (`000000`), ascending or descending runs (`123456`, `654321`), and a configurable denylist.
- **FR-PIN-3** **Set** (`POST /v1/staff/me/pin`) requires a `staff` session, which means a fresh OTP login. A PIN can never be set or changed from a PIN-unlocked POS session.
- **FR-PIN-4** Store PINs as `argon2id` hashes in `staff.pin_hash`.
- **FR-PIN-5** **Reset** (`DELETE /v1/staff/{id}/pin`) clears a PIN. It requires `staff.manage` at an outlet where the target holds a role. The staff member must then set a new PIN via OTP.
- **FR-PIN-6** **PINs are not unique and do not identify a person.** The POS unlock flow is *select your name → enter PIN*. This avoids a "PIN already taken" leak.
- **FR-PIN-7** **No lockout.** A wrong PIN never blocks the staff member or the device. Each failure is recorded locally and synced as `pin.failed` (staff ID, device ID, timestamp) for reporting (§7).
- **FR-PIN-8** The PIN is a local unlock only. No API endpoint accepts a PIN as a credential.

### 3.6 POS device enrollment and session

- **FR-DEV-1** **Enroll** (`POST /v1/auth/devices/enroll`) has two steps, both performed on the device:
  1. A staff member with `device.enroll` at the outlet completes OTP with `purpose = device_enroll`.
  2. The device submits `{ outlet_id, name, device_fingerprint }`.
- **FR-DEV-2** Enrollment creates a `devices` row with `enrolled_by` set and starts a `pos` refresh family bound to that `device_id`. It writes `device.enrolled` to both `auth_events` and `audit_logs`.
- **FR-DEV-3** **One active device per outlet** (configurable, default 1). If an active device already exists, enrollment returns `409 device_limit_reached` unless the request sets `replace: true`. With that flag, the existing device is revoked in the same transaction.
- **FR-DEV-4** **Roster bundle** (`GET /v1/pos/roster`): for each staff member with a role at the device's outlet, return:
  - staff ID, display name, and PIN hash
  - permission set at this outlet
  - a roster version

  The POS verifies PINs and step-up permissions locally against this bundle. Fetch it on every sync and at least every 15 minutes while online.
- **FR-DEV-5** **Revoke** (`POST /v1/auth/devices/{id}/revoke`) sets `is_active = false`, deletes the device's refresh family, and writes `session.revoked`. The device's next refresh or sync fails with `401 device_revoked`.
- **FR-DEV-6** On `device_revoked`, the POS:
  - stops accepting new sales
  - wipes the roster bundle
  - **keeps** its unsynced queue encrypted on disk

  Re-enrolling the same physical device (same fingerprint, fresh OTP) restores sync. Queued operations then replay under the original `device_id` with an `audit_logs` flag `sync.after_reenroll`.
- **FR-DEV-7** POS API calls carry the `pos` access token plus `X-Acting-Staff-Id`. Offline-replayed operations carry `acting_staff_id` per operation in the sync payload, as recorded at action time.
- **FR-DEV-8** Update `devices.last_seen_at` on every successful refresh or sync.

### 3.7 Supervisor step-up on POS

- **FR-STEP-1** These actions require step-up when the acting staff member lacks the permission:
  - void after payment (`order.void.paid`)
  - discount (`order.discount`)
  - price override (`order.price_override`)
  - shift open/close (`shift.open`, `shift.close`)
- **FR-STEP-2** For step-up, the POS shows *select approver → approver enters own PIN*. It checks the PIN and the approver's permission against the local roster. This works offline.
- **FR-STEP-3** Every step-up operation records both `acting_staff_id` and `approved_by`. These map to `audit_logs.actor_id` and the new `audit_logs.approved_by` (§8), and to `order_lines.voided_by` for voids.
- **FR-STEP-4** A step-up approval covers exactly one action. It is never cached for the rest of the session.
- **FR-STEP-5** If the acting staff member already holds the permission, no step-up is needed. The actor is also the approver.
- **FR-STEP-6** On sync, the server re-checks that the approver held the permission at that outlet, using the role assignments in effect at the operation's timestamp.
  - If the check fails, the operation is **still accepted**, because the sale or void already happened physically.
  - It is flagged `authz.offline_violation` in `audit_logs` and surfaced in the void/discount report.

### 3.8 Authorization

- **FR-AZ-1** Permissions are strings. Roles map to permission sets via `roles.permissions`. The v1 matrix:

  | Permission | Cashier | Supervisor | Manager | Owner |
  | --- | :-: | :-: | :-: | :-: |
  | `order.create`, `payment.record` | ✓ | ✓ | ✓ | ✓ |
  | `order.void.unpaid` | ✓ | ✓ | ✓ | ✓ |
  | `product.sold_out.toggle` | ✓ | ✓ | ✓ | ✓ |
  | `order.void.paid`, `order.discount`, `order.price_override` | | ✓ | ✓ | ✓ |
  | `shift.open`, `shift.close`, `shift.report.view` | | ✓ | ✓ | ✓ |
  | `refund.request` | | ✓ | ✓ | ✓ |
  | `refund.approve` | | | ✓ | ✓ |
  | `product.availability.manage`, `stock.manage` | | | ✓ | ✓ |
  | `staff.manage` (outlet-scoped, see FR-AZ-6) | | | ✓ | ✓ |
  | `device.enroll`, `device.revoke` | | | ✓ | ✓ |
  | `report.outlet.view` | | | ✓ | ✓ |
  | `report.all.view`, `cost.view`, `margin.view` | | | | ✓ |
  | `catalog.manage`, `outlet.manage` | | | | ✓ |
  | `audit.view`, `auth_events.view` | | | | ✓ |

- **FR-AZ-2** Outlet-scoped permissions are checked against the target resource's `outlet_id`. Holding a permission at Outlet A grants nothing at Outlet B.
- **FR-AZ-3** Middleware resolves `{outlet_id → permissions}` from `staff_outlet_roles` and `staff.is_active`. The result is cached in Redis at `authz:staff:{staff_id}` and invalidated on any role or active-status change.
- **FR-AZ-4** Deactivating a staff member:
  - sets `is_active = false`
  - revokes all their refresh families
  - invalidates the authz cache

  Their outstanding access tokens then fail authorization on the next request.
- **FR-AZ-5** A request for a resource at an outlet where the caller has no role returns `404 not_found`, not `403`. This avoids disclosing that the resource exists. A request at an outlet where the caller has a role but lacks the permission returns `403 forbidden`.
- **FR-AZ-6** **No privilege escalation:**
  - A manager can grant or revoke only `cashier` and `supervisor` at outlets where they hold `manager`.
  - Only an owner can grant `manager` or `owner`.
  - Nobody can change their own roles.
- **FR-AZ-7** **Refund separation of duties:** `refunds.approved_by` must differ from `requested_by`. Owners are exempt (OQ-3).
- **FR-AZ-8** **Global permissions** cover catalog, outlets, cross-outlet reports, and audit. They come from a global role grant (§8, OQ-2), not from an outlet-scoped row.
- **FR-AZ-9** **Customer scope:** a `customer` token can read and modify only orders where `customer_id = sub`, and its own profile.
- **FR-AZ-10** **Guest orders:** `GET /v1/track/{tracking_token}` is public. It returns only status, items, totals, and outlet. `tracking_token` is at least 128 bits of randomness.

### 3.9 Customer account specifics

- **FR-CUS-1** **Guest linking:** when a customer verifies a phone number, attach every order with `guest_phone` equal to that number and `customer_id IS NULL`. Write one audit entry with the count.
- **FR-CUS-2** **Add identifier** (`POST /v1/me/identifiers`): a signed-in customer can add the identifier type they lack. It must be verified by OTP to the new identifier. If the new identifier already belongs to another customer, return `409 identifier_in_use`. There is no automatic merge in v1.
- **FR-CUS-3** **Change identifier:** this is the same flow as adding. The old value is replaced only after the new one verifies. A customer must always keep at least one identifier.
- **FR-CUS-4** If a person is both staff and customer with the same phone, the two populations stay fully separate. `purpose` and audience keep their sessions independent.

### 3.10 Audit

- **FR-AUD-1** `auth_events` records these events with IP, user agent, and device where applicable:
  - `otp.requested`, `otp.verified`, `otp.failed`
  - `token.refreshed`, `token.reuse_detected`
  - `device.enrolled`, `session.revoked`
  - `pin.set`, `pin.reset`, `pin.failed`
- **FR-AUD-2** `audit_logs` records `role.granted`, `role.revoked`, `staff.deactivated`, `device.enrolled`, `device.revoked`, and every step-up action with `approved_by`.
- **FR-AUD-3** Both tables are append-only. The application role has no `UPDATE` or `DELETE` grant on them.

---

## 4. Non-Functional Requirements

### 4.1 Security

- **NFR-SEC-1** All randomness (OTP codes, refresh tokens, tracking tokens, decoys) comes from `crypto/rand`.
- **NFR-SEC-2** Keys are loaded from the secret store and never committed or logged:
  - the OTP HMAC secret
  - the JWT signing keys
  - the refresh-hash pepper, if used

  These are three independent secrets.
- **NFR-SEC-3** JWT signing keys rotate at least every 90 days without downtime (FR-TOK-10).
- **NFR-SEC-4** Validate `alg` against an allow-list (`ES256` only). Reject `none` and any HMAC algorithm. Allow at most 60 seconds of clock leeway.
- **NFR-SEC-5** Logs never contain OTP codes, refresh tokens, access tokens, PINs, or PIN hashes. Identifiers are masked in logs (`+62812****789`, `a***@domain.com`). Full identifiers live only in `auth_events`.
- **NFR-SEC-6** Browser clients (online shop, dashboard) receive the refresh token in an `HttpOnly; Secure; SameSite=Strict` cookie scoped to `/v1/auth`, and never in JavaScript-readable storage. The POS stores it in the platform keystore.
- **NFR-SEC-7** The POS encrypts the roster bundle and the offline queue at rest, with a key held in the platform keystore. The roster contains PIN hashes, and a 6-digit space is brute-forceable offline, so this encryption is the main protection for PINs (§9).
- **NFR-SEC-8** Every OTP request takes the same server-side code path up to the async dispatch. The p95 latency difference between existing and non-existing identifiers is under 10 ms.

### 4.2 Performance

- **NFR-PERF-1** Token validation plus authorization adds at most 5 ms at p95 per request, with a warm authz cache.
- **NFR-PERF-2** `otp/request`, `otp/verify`, and `token/refresh` respond in under 200 ms at p95, excluding delivery.
- **NFR-PERF-3** WhatsApp OTP is delivered within 10 seconds at p95 of the request. Email is delivered within 30 seconds at p95.
- **NFR-PERF-4** POS PIN unlock and step-up complete locally in under 500 ms on target hardware. The argon2id parameters are tuned to meet this.

### 4.3 Availability and resilience

- **NFR-AV-1** Redis runs with AOF enabled (`appendfsync everysec`). Losing Redis must not lose more than 1 second of refresh-token writes.
- **NFR-AV-2** When Redis is unavailable:

  | Operation | Behaviour | Reason |
  | --- | --- | --- |
  | OTP request and verify, token refresh, enrollment | **Fail closed**: `503 auth_unavailable` | Cannot check state |
  | Access-token validation | Continues, because it is stateless | |
  | `revoked:{jti}` denylist check | **Fails open**, with an alert | Bounded by the 15-minute token life |
  | Authz cache miss | Falls back to Postgres | |

- **NFR-AV-3** POS sales never depend on server or Redis availability. Only sync does.
- **NFR-AV-4** Notifier failures are retried with backoff for up to 2 minutes, within the 5-minute OTP TTL, before the dispatch is marked permanently failed.

### 4.4 Privacy and compliance

- **NFR-PRV-1** Phone numbers and emails are personal data under Indonesia's PDP Law (UU 27/2022). Collect them only for authentication and order contact, and restrict `auth_events.view` to owners.
- **NFR-PRV-2** Retain `auth_events` for 12 months (configurable), then purge or anonymize.

### 4.5 Operability and testability

- **NFR-OPS-1** All limits are configuration, not code: TTLs, cooldown, quotas, attempt limits, lifetimes, grace window, and device limit.
- **NFR-OPS-2** Auth emits Prometheus metrics for every item in §7.
- **NFR-OPS-3** Integration tests run against real Postgres and Redis via `testcontainers-go`. The notifier and clock are injectable, so TTL and expiry paths are deterministic in tests.
- **NFR-OPS-4** The OpenAPI spec documents every auth endpoint and error code in §5.

---

## 5. Edge Cases and Error Handling

### 5.1 Error contract

Every error uses one body shape:

```json
{ "error": { "code": "otp_cooldown", "message": "Please wait before requesting another code.", "retry_after": 42 } }
```

| HTTP | `code` | When | Notes |
| --- | --- | --- | --- |
| 400 | `invalid_identifier` | Malformed phone or email | Format check only |
| 400 | `weak_pin` | PIN fails FR-PIN-2 | |
| 401 | `otp_invalid` | Any verify failure | One code for all causes (FR-VER-4) |
| 401 | `token_expired` | Access token past `exp` | Client should refresh |
| 401 | `token_invalid` | Bad signature, wrong `aud`, malformed, denylisted | Never says *why* |
| 401 | `session_revoked` | Refresh token missing, expired, or family revoked | Client must re-authenticate |
| 401 | `device_revoked` | POS device revoked | POS enters FR-DEV-6 state |
| 403 | `forbidden` | Has a role at the outlet, lacks the permission | |
| 404 | `not_found` | Resource at an outlet where the caller has no role | FR-AZ-5 |
| 409 | `device_limit_reached` | Enrolling over the device limit without `replace` | |
| 409 | `identifier_in_use` | Adding an identifier owned by another customer | |
| 409 | `refund_self_approval` | Approver equals requester | |
| 429 | `otp_cooldown` | Within 60 s of the last send | Includes `Retry-After` |
| 429 | `otp_quota_exceeded` | Identifier or IP quota exhausted | Includes `Retry-After` |
| 429 | `rate_limited` | Verify or other endpoint limiter | |
| 503 | `auth_unavailable` | Redis or another hard dependency is down | |

### 5.2 Edge cases

**OTP**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-1 | Unknown staff identifier requests OTP | 202, nothing sent, decoy record stored, `otp.requested` with null subject |
| E-2 | Unknown staff identifier then tries to verify | `401 otp_invalid` for 5 attempts, then the same error. Indistinguishable from a real record |
| E-3 | Inactive staff member, or one with no roles, requests OTP | Treated exactly like E-1 |
| E-4 | Two verify requests with the correct code arrive concurrently | Exactly one succeeds. The other gets `otp_invalid` |
| E-5 | User requests a new code, then enters the old one | `otp_invalid`, because the old code was replaced |
| E-6 | Code verifies at 4:59 and the request is processed at 5:01 | Expiry is evaluated by Redis TTL at processing time, so the result is `otp_invalid` |
| E-7 | WhatsApp is down and the customer has no email | 202 returned. Dispatch fails, the quota slot is refunded, and the user can retry after the cooldown |
| E-8 | Same phone typed as `0812…` and `+62812…` | Normalized to one identifier, so cooldown and quota are shared |
| E-9 | Attacker sprays OTP requests across many numbers from one IP | Stopped by the IP quota at 20/hour |
| E-10 | Attacker spreads requests across many IPs to one number | Stopped by the identifier quota at 5/day and the cooldown |
| E-11 | Person is both staff and customer on one phone | Separate `purpose` keys and audiences. Cooldown and quota are shared, by design, to bound cost |

**Tokens and sessions**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-12 | POS refreshes, the response is lost, and the POS retries with the old token within 30 s | Same successor pair returned. No revocation (FR-TOK-7) |
| E-13 | Old refresh token replayed after 30 s, or from a different device | Family revoked, `token.reuse_detected` logged, `401 session_revoked` |
| E-14 | Customer token sent to a staff route | `401 token_invalid` |
| E-15 | Staff member deactivated while holding a valid access token | Next request fails authorization: `403`, or `404` for outlet-scoped resources |
| E-16 | Role revoked mid-session | Effective on the next request, through cache invalidation |
| E-17 | Signing key rotated mid-session | Old tokens validate until expiry, because the retired key stays in JWKS |
| E-18 | Redis flushed or lost | Everyone must re-authenticate. POS devices must re-enroll. This is the documented consequence, and AOF makes it rare |
| E-19 | Staff dashboard session hits 12 h during a long shift | Refresh fails with `session_revoked` and the user re-OTPs. In-flight form data is the client's responsibility |

**POS offline**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-20 | POS offline when the access token expires | Sales continue. On reconnect: refresh, fetch roster, then replay the sync batch |
| E-21 | POS offline for more than 90 days | Refresh token expired. Device must re-enroll. Queue is preserved and replayed (FR-DEV-6) |
| E-22 | Staff deactivated at HQ while the POS is offline | They can still unlock offline until the next roster sync. Operations after `deactivated_at` are accepted and flagged `authz.offline_violation` |
| E-23 | Supervisor demoted while the POS is offline, then approves a void | Accepted on sync and flagged (FR-STEP-6) |
| E-24 | Device revoked while it has unsynced orders | Queue kept encrypted. Re-enrollment replays it under the original `device_id`, flagged `sync.after_reenroll` |
| E-25 | Lost or stolen device | Revoke it. The thief can still unlock it with a known PIN and record fake sales locally, but these can never sync, because the server rejects the revoked family |
| E-26 | Many wrong PINs on one device | No lockout (FR-PIN-7). Every failure is synced and appears in the PIN-failure metric and report |
| E-27 | Staff member has no PIN set yet | Shown as "PIN not set" in the unlock list. They must set one via OTP before using the POS |
| E-28 | Manager enrolls a replacement device while the old one is still active | `409` unless `replace: true`, which revokes the old device atomically |

**Authorization and accounts**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-29 | Manager tries to grant `manager` to someone | `403 forbidden` |
| E-30 | Manager tries to change their own roles | `403 forbidden` |
| E-31 | Manager at Outlet A requests an Outlet B report | `404 not_found` |
| E-32 | Customer adds an email already used by another account | `409 identifier_in_use`, no merge |
| E-33 | Guest orders exist under a phone, and the customer signs up by email only | No linking yet. Linking happens when they add and verify that phone |
| E-34 | Supervisor requests a refund, then tries to approve it | `409 refund_self_approval`, unless they are an owner |

---

## 6. Acceptance Criteria

Each criterion is an automated integration test unless marked *(manual)*.

### OTP

- **AC-1** *(FR-OTP-2)* After `otp/request`, the Redis value at `otp:{purpose}:{identifier}` contains no substring equal to the issued code. TTL is 300 s ± 1.
- **AC-2** *(FR-OTP-4)* A second request for the same identifier within 60 s returns `429 otp_cooldown` with `Retry-After` ≤ 60. No message is dispatched.
- **AC-3** *(FR-OTP-5)* After 5 successful sends to one identifier in a day, the 6th returns `429 otp_quota_exceeded`, even with a different `purpose` and a different IP.
- **AC-4** *(FR-OTP-6)* The 21st request from one IP within an hour returns `429`, regardless of identifier.
- **AC-5** *(FR-OTP-8, FR-OTP-12, NFR-SEC-8)* For a known staff phone and an unknown phone:
  - response status, body, and headers are byte-identical
  - the notifier is called only for the known one
  - p95 latency over 200 runs differs by under 10 ms
- **AC-6** *(FR-VER-2, FR-VER-4)* Five wrong codes each return `otp_invalid`. The correct code on the 6th attempt also returns `otp_invalid`.
- **AC-7** *(FR-VER-6)* For an unknown staff identifier, verify returns the same error codes, in the same sequence, as AC-6.
- **AC-8** *(FR-VER-3)* 20 concurrent verify calls with the correct code produce exactly one success and one issued session.
- **AC-9** *(FR-VER-7, FR-CUS-1)* When a new phone verifies under `customer_login`:
  - a customer is created with `verified_at` set
  - all prior guest orders with that phone get its `customer_id`
- **AC-10** *(FR-OTP-10)* If WhatsApp dispatch fails permanently for a staff member who has an email, the email notifier is called once with a new delivery of the same code.

### Tokens

- **AC-11** *(FR-TOK-1, FR-TOK-2)* A decoded access token has the required claims, `alg = ES256`, `exp − iat = 900`, and no role or permission claims.
- **AC-12** *(FR-TOK-5)* A refresh returns a new pair. Presenting the previous refresh token 31 s later revokes the family. The new token then also fails with `session_revoked`.
- **AC-13** *(FR-TOK-7)* Presenting the previous refresh token within 30 s, from the same device, returns the identical successor pair. No revocation happens.
- **AC-14** *(FR-TOK-4)* A customer family is rejected 30 days after `family_started_at`, even if refreshed daily. A POS family refreshed every 80 days remains valid indefinitely.
- **AC-15** *(FR-TOK-11)* Each audience presented to each other audience's route group returns `401 token_invalid`. All 6 combinations are tested.
- **AC-16** *(FR-TOK-8)* After logout, the old access token is rejected immediately, and the refresh token returns `session_revoked`.
- **AC-17** *(NFR-SEC-4)* Tokens signed with `none`, with HS256 using the public key as secret, or with an unknown `kid` are all rejected.

### Devices, PIN, and step-up

- **AC-18** *(FR-DEV-1, FR-DEV-2)* A manager completing `device_enroll` OTP and enrollment produces:
  - one `devices` row
  - one `pos` refresh family
  - `device.enrolled` entries in both audit tables

  A cashier attempting the same receives no code (AC-5 behaviour), and enrollment fails.
- **AC-19** *(FR-DEV-3)* A second enrollment at the same outlet returns `409`. With `replace: true` it succeeds, and the first device's next refresh returns `device_revoked`.
- **AC-20** *(FR-DEV-4)* The roster contains exactly the outlet's active staff, with their permissions at that outlet. It contains no staff from other outlets.
- **AC-21** *(FR-PIN-2, FR-PIN-3)* Setting `123456` returns `400 weak_pin`. Setting a PIN with a `pos` token returns `401 token_invalid`.
- **AC-22** *(FR-STEP-3)* A synced void-after-payment by a cashier, approved by a supervisor, stores the cashier as `actor_id`, the supervisor as `approved_by`, and the supervisor in `order_lines.voided_by`.
- **AC-23** *(FR-STEP-6)* A synced operation whose approver lacked the permission at action time is persisted and flagged `authz.offline_violation`.
- **AC-24** *(FR-PIN-7)* Synced `pin.failed` events appear in `auth_events` with the correct staff and device IDs. No endpoint or setting blocks the staff member.
- **AC-25** *(manual)* On target POS hardware, PIN unlock completes in under 500 ms. The roster and queue files on disk are not readable without the keystore key.

### Authorization

- **AC-26** *(FR-AZ-1)* A table-driven test asserts every cell of the permission matrix: each role × each permission, allowed or denied.
- **AC-27** *(FR-AZ-2, FR-AZ-5)* A person who is a manager at A and a cashier at B:
  - can view A's reports
  - gets `403` for B's reports
  - gets `404` for C's reports
- **AC-28** *(FR-AZ-4)* After deactivation, the staff member's next request with a still-valid access token is denied, and all their refresh tokens return `session_revoked`.
- **AC-29** *(FR-AZ-6)* Managers cannot grant `manager` or `owner`, and nobody can edit their own roles.
- **AC-30** *(FR-AZ-7)* Same-person refund approval returns `409` for a manager and succeeds for an owner.
- **AC-31** *(FR-AZ-9)* Customer A requesting customer B's order by ID receives `404`.

### Audit and logs

- **AC-32** *(NFR-SEC-5)* A log-scrubbing test runs the full auth suite and greps all captured log output. It finds no OTP code, token, PIN, or unmasked identifier.
- **AC-33** *(FR-AUD-3)* The application database role cannot `UPDATE` or `DELETE` rows in `auth_events` or `audit_logs`.

---

## 7. Metrics

Exposed as Prometheus metrics, labelled by `channel`, `purpose`, `outlet_id`, or `audience` where meaningful.

### 7.1 Health and experience

| Metric | Definition | Target / alert |
| --- | --- | --- |
| OTP delivery success rate | Delivered ÷ dispatched, per channel | ≥ 98%; alert below 95% over 15 min |
| OTP delivery latency | Request → provider-confirmed delivery | p95 ≤ 10 s on WhatsApp, ≤ 30 s on email |
| OTP conversion | Verified sessions ÷ OTP requests for known identifiers | ≥ 85%; a drop signals delivery or UX problems |
| Mean attempts to verify | Verify calls per successful verify | ≤ 1.3 |
| Auth endpoint latency | p95 of request, verify, and refresh | ≤ 200 ms |
| Authz overhead | p95 middleware time | ≤ 5 ms |
| Refresh failure rate | `session_revoked` ÷ refresh calls, excluding expiry | < 0.5% |
| POS re-enrollments per device per month | Unplanned re-enrollments | ≤ 1; higher means the grace window or sliding expiry is not working |

### 7.2 Cost

| Metric | Definition | Target / alert |
| --- | --- | --- |
| WhatsApp OTP sends per day | Count and estimated cost | Budget alert at a configurable daily ceiling |
| Cost per successful login | WhatsApp spend ÷ verified sessions | Track the trend; alert on a 2× week-over-week rise |
| Sends to unknown customer identifiers | Signups that never verify | Alert above 30% of customer sends; possible SMS-pumping-style abuse |

### 7.3 Security signals

| Metric | Definition | Alert |
| --- | --- | --- |
| Quota and cooldown rejections | 429s per limiter | Spike > 5× baseline in 1 h |
| Token reuse detections | `token.reuse_detected` count | Any occurrence on a `pos` or `staff` family pages on-call; customer spikes > 3× baseline |
| Failed PIN attempts | Per staff member and per device, per shift | Report any staff or device with ≥ 10 failures in a shift (no lockout, so visibility is the control) |
| Step-up approvals | Count per approver, per outlet, per shift | Surfaced in the void/discount report; outliers by approver |
| Offline authz violations | `authz.offline_violation` count | Any occurrence appears on the owner dashboard |
| Device lifecycle | Enrollments, revocations, `sync.after_reenroll` | Any revocation notifies owners |
| Redis denylist fail-open | Seconds spent in fail-open mode | Any occurrence pages on-call |

---

## 8. Required Changes to the Current Data Model

**Postgres**

```sql
-- Customers may sign up with phone OR email (confirmed decision).
ALTER TABLE customers ALTER COLUMN phone DROP NOT NULL;
ALTER TABLE customers ADD CONSTRAINT customers_identifier_present
  CHECK (phone IS NOT NULL OR email IS NOT NULL);

-- Global (non-outlet) role grants, e.g. owner. See OQ-2.
CREATE TABLE staff_global_roles (
  staff_id UUID NOT NULL REFERENCES staff(id),
  role_id  UUID NOT NULL REFERENCES roles(id),
  PRIMARY KEY (staff_id, role_id)
);

-- Supervisor step-up: record the approver separately from the actor.
ALTER TABLE audit_logs ADD COLUMN approved_by UUID REFERENCES staff(id);

-- Devices as auth subjects; track revocation explicitly.
ALTER TABLE devices ADD COLUMN revoked_at TIMESTAMPTZ;
ALTER TABLE devices ADD COLUMN revoked_by UUID REFERENCES staff(id);

-- Needed to evaluate offline operations against roles at action time (FR-STEP-6).
ALTER TABLE staff ADD COLUMN deactivated_at TIMESTAMPTZ;
-- Role grants need history: either soft-delete with granted_at/revoked_at
-- on staff_outlet_roles, or derive from audit_logs role.granted/role.revoked.
```

- `auth_events.subject_type` gains `device`.
- `auth_events.event` gains `pin.set`, `pin.reset`, `pin.failed`.

**Redis**: additions to the key schema in §B of the overview.

```
otp:verify:ip:{ip}            → counter                       TTL 1h
refresh:successor:{old_hash}  → {access, refresh}             TTL 30s
authz:staff:{staff_id}        → {outlet_id: [perms], global}  TTL 10m, invalidated on change
```

**API**: additions to the overview's API surface.

```
GET    /.well-known/jwks.json
POST   /v1/auth/sessions/revoke-all
GET    /v1/pos/roster
DELETE /v1/staff/{id}/pin
POST   /v1/me/identifiers
```

---

## 9. Risks and Accepted Trade-offs

| Risk | Status |
| --- | --- |
| **No PIN lockout.** Anyone holding an unlocked POS device can try PINs freely. | Accepted. The device is already trusted, and the PIN gates attribution, not access to money. Mitigated by failed-PIN reporting (§7.3) and full action attribution. |
| **PIN hashes on the device.** A 6-digit PIN hash can be brute-forced in seconds if the roster file is extracted. | Mitigated by keystore-backed encryption at rest (NFR-SEC-7). A compromised device must be revoked, and its staff should reset their PINs. |
| **Offline role staleness.** A deactivated or demoted person can act until the next roster sync. | Accepted. Bounded by the sync interval while online; flagged after sync (FR-STEP-6). |
| **OTP-to-email for customers** creates a second sign-up route that is cheaper to abuse. | IP and identifier quotas apply equally; unknown-identifier send rate is monitored (§7.2). |
| **Redis loss** forces global re-authentication and POS re-enrollment. | AOF persistence. POS queues survive re-enrollment (FR-DEV-6). |

---

## 10. Open Questions

These have a working assumption in this spec. Confirm or change them before implementation.

| # | Question | Working assumption |
| --- | --- | --- |
| OQ-1 | Add a third JWT audience `pos` for device sessions, or fold devices into `staff`? | Separate `pos` audience (§2) |
| OQ-2 | How are owner-level and global permissions represented? `staff_outlet_roles.outlet_id` is `NOT NULL`. | New `staff_global_roles` table (§8) |
| OQ-3 | Can an owner approve their own refund? Small outlets may have only one senior person. | Owners exempt; managers are not |
| OQ-4 | Should customer and staff refresh lifetimes be absolute or sliding? The overview marks only POS as sliding. | Absolute for customer (30 d) and staff (12 h) |
| OQ-5 | Who may set or reset another staff member's PIN: manager only, or supervisor too? | Manager and owner only |
| OQ-6 | Should a cashier be able to request refunds, or only supervisors and above? | Supervisor and above |
| OQ-7 | Should the device limit per outlet stay hard-coded at 1, or be per-outlet config? | Global config, default 1 |
| OQ-8 | When a customer signs up by email and later adds a phone that has guest orders, link them automatically? | Yes, on successful phone verification |
