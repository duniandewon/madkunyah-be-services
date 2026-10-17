# Current Feature

<!-- Feature name and short description -->

**Authorization & staff management (auth slice 2)** — role-based, outlet-scoped permission checks for staff routes, plus the staff endpoints needed to manage access: create staff, grant/revoke roles with escalation rules, deactivate, and revoke a staff member's sessions. Spec: `context/features/auth-spec.md` §3.8, FR-TOK-9, FR-AUD-2, §8.

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
