# Current Feature

<!-- Feature name and short description -->

**POS devices, staff PIN & roster (auth slice 3)**: OTP-gated POS device enrollment that starts a device-bound `pos` refresh session, staff PIN set/reset, the roster bundle the POS uses to unlock offline, device revoke, and `pin.failed` event intake. Spec: `context/features/auth-spec.md` §3.5, §3.6, FR-AUD-2, §5.1, AC-18–21, AC-24.

## Status

<!-- Not started | In Progress | Completed -->

Completed

## Goals

<!-- Goals and requirements -->

## Notes

<!-- Any extra notes -->

## History

<!-- Keep this updated from earliest to latest -->

- **2026-10-13** — Initial setup: HTTP skeleton (chi), typed env config, Postgres + Redis connections, `/health` and `/ready` endpoints.
- **2026-10-13** — Added project context docs (`CLAUDE.md`, `context/`): project spec, coding standards, AI interaction guidelines, and this feature tracker.
- **2026-10-16** — Auth slice 1 (`feature/auth-otp-sessions`): migration `0001_auth` with role seed, phone/email identifiers, OTP request/verify with cooldown, quotas, decoys and attempt limits, ES256 access tokens + JWKS, rotating refresh sessions with reuse detection and grace window, logout, revoke-all, audience middleware, log-stub notifier. Dev DB reset to this repo's migrations; coding standards updated to pgx; golangci-lint clean. Deviations: `device_enroll` deferred, revoke-all shared by customer/staff, full identifier in `auth_events`, logout revokes whole family.
- **2026-10-17** — Auth slice 2 (`feature/authz`): outlet-scoped authorization (`internal/authz` resolver with Redis cache at `authz:staff:{id}`, `AUTHZ_CACHE_TTL`, Postgres fallback; `LoadStaffAccess` / `RequireOutletPermission` middleware — no role at outlet → 404, missing permission → 403, deactivated → 403). Staff endpoints (`internal/staff`): list/create outlet staff, grant/revoke outlet and global roles, deactivate (revokes sessions + cache), admin session revoke. FR-AZ-6 escalation rules checked by role code. Migration `0002_authz` adds `audit_logs`; role and deactivation changes audited in the same transaction, which also serves as role-grant history. Verified: unit tests, AC-26 matrix test, authz E2E 41/41, slice 1 regression 39/39, golangci-lint clean. Out of scope: FR-AZ-7 (refunds), FR-AZ-9/10 (orders), FR-AUD-3 DB grants, full staff CRUD. Remaining auth slices: devices + PIN + roster → customer identifiers → step-up server side.
- **2026-10-19** — Auth slice 3 (`feature/devices-pin-roster`): staff PIN set/reset (`POST /v1/staff/me/pin`, `DELETE /v1/staff/{id}/pin`; 6 digits, weak-PIN rules + `PIN_DENYLIST`, argon2id PHC hash with `PIN_ARGON2_*` params, ~17 ms on the dev machine; `pin.set`/`pin.reset` in `auth_events`, reset actor in `audit_logs`). `device_enroll` OTP purpose gated on `device.enroll` at the target outlet; `POST /v1/auth/devices/enroll` redeems the code, enforces `DEVICE_LIMIT_PER_OUTLET` (409 `device_limit_reached`, or `replace` revokes the oldest in the same transaction; the code survives a 409), re-enrolls a known fingerprint under its original `device_id`, and opens a device-bound `pos` session (subject = device). `POST /v1/auth/devices/{id}/revoke` (`device.revoke`); Redis `refresh:device_revoked:{hash}` markers make the next refresh return 401 `device_revoked`, and refresh plus every `/v1/pos` route also check `devices.is_active` in Postgres. `last_seen_at` updated on refresh. `internal/pos`: `GET /v1/pos/roster` (outlet staff + global role holders, PIN hashes, permissions, content-hash version) and `POST /v1/pos/pin-failures` (client-generated IDs, idempotent). `RequireActingStaff` middleware for `X-Acting-Staff-Id`, not mounted yet. No migration. Verified: unit tests, AC-26 matrix test, slice 3 E2E 81/81, slice 1 smoke 8/8, golangci-lint clean. Out of scope: server-side step-up (FR-STEP-*), `/v1/sync/batch` and `sync.after_reenroll`, `authz.offline_violation`, device listing, POS-side encryption (NFR-SEC-7). Remaining auth slices: customer identifiers → step-up server side.
