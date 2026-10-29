# Current Feature: Catalog Core

<!-- Feature name and short description -->

Global menu structure (categories, products, variants, modifier groups, modifiers), owner dashboard endpoints to maintain it, and the internal `Quote` interface that validates, prices and snapshots order lines. Spec: `context/features/catalog-core-specs.md`.

## Status

<!-- Not started | In Progress | Completed -->

In Progress

## Goals

<!-- Goals and requirements -->

- Migration `catalog_core` per spec §8: `categories`, `products`, `product_variants`, `modifier_groups`, `modifiers`, `product_modifier_groups` with partial unique indexes on active names and SKUs; `audit_logs.entity_type` gains the five catalog entity types
- Categories (FR-CAT-1..5): global, unique active names (`409 name_taken`), archiving hides products without archiving them, products may live in archived categories, ordered by `sort_order` then name
- Products (FR-PRD-1..9): field limits, per-category unique names, SKUs unique across products and variants including archived (`409 sku_taken`, checked in the service), integer rupiah prices `0..CATALOG_PRICE_MAX` (strings/floats/negatives → `400 validation_failed`), web/POS visibility defaults
- Cost visibility: writing `cost_price` needs `catalog.manage` + `cost.view`; field absent (not null) for callers without `cost.view`
- Archive, never delete: archive/restore via `PATCH {is_active}`, lists hide archived unless `include_archived=true`, restore re-checks name uniqueness
- Variants (FR-VAR-1..6): single axis, `price_delta ≥ 0`, unique active names per product, archiving the last active variant → `409 last_active_variant`
- Modifier groups/modifiers (FR-MOD-1..7): global reusable groups, `min_select`/`max_select` rules with derived `required`, unique active modifier names per group, satisfiability guard (`409 modifier_group_unsatisfiable` with affected products) on attach / raise `min_select` / archive modifier, `PUT /v1/products/{id}/modifier-groups` replaces atomically (archived group → 400), archiving an attached group → `409 modifier_group_in_use`
- Availability windows (FR-AVL-1..5): daily `[from, to)` in outlet timezone, wraps past midnight, both-or-neither and `from ≠ to`, evaluated at `Quote.At`, injectable clock
- `catalog.Quote(ctx, q, req)` (FR-QTE-1..7): reads Postgres inside the caller's transaction, fixed query count regardless of lines, `ErrOutletUnavailable` for unknown/inactive outlet, per-line problem kinds (`product_unavailable`, `outside_window`, `variant_required`, `variant_invalid`, `modifier_invalid`, `modifier_count_invalid`), snapshot with price breakdown and `unit_cost`, `Strict` vs `Record` modes
- Dashboard reads (FR-READ-1..4): list/detail for categories, products (filters `category_id`, `q`, `include_archived`; ordered by category order, sort_order, name), modifier groups with modifiers; detail includes archived children marked
- Authorization (§3.8): reads need any role (outlet or global), writes need global `catalog.manage`
- Audit (FR-CAUD-1..3): one `audit_logs` row per changing write in the same transaction, changed fields only, `value_delta` on price changes, no row for no-op PATCH
- New error codes: `name_taken`, `sku_taken`, `last_active_variant`, `modifier_group_unsatisfiable`, `modifier_group_in_use`, `catalog_unavailable` (503 when Postgres down)
- 64 KB body cap, length limits on every string, `CATALOG_PRICE_MAX` config
- Prometheus metrics (§7): Quote latency by line bucket, Quote problems by kind/mode, catalog write rate, price changes
- Acceptance criteria AC-1..AC-19 covered by tests; table-driven unit tests for selection/resolution rules

## Notes

<!-- Any extra notes -->

- Depends on `internal/authz` (incl. global roles). No step-up dependency. No new permission strings: `catalog.manage` and `cost.view` already exist in FR-AZ-1.
- §8 DDL supersedes the Catalog section of the overview DDL. `product_stock_links` not created here (inventory owns it); `image_key` comes with Product Images.
- Prices are tax-exclusive (PB1 and service charge added by orders).
- `Quote` always reads Postgres, never a cache, so the committed price is the checked price. Outlet Catalog will extend it with overrides/sold-out without changing callers.
- Decided at start: integration tests are DB-gated on `TEST_DATABASE_URL` (migrated by `TestMain`, dev DB `madkunyah_test`) instead of testcontainers-go; Prometheus metrics (§7) and OpenAPI (NFR-OPS-4) deferred.
- `validation_failed` reuses the existing `invalid_input` code. Error envelope gains `details` (affected products on the two 409s).
- Catalog permissions are checked globally (`CanGlobal`): an outlet-scoped owner can read but not write the catalog, and does not see cost.
- Beyond the spec's three satisfiability triggers, restoring a product or a group also re-checks satisfiability.
- `value_delta` is set on edits only, not on creation.
- PUT modifier-groups body is `{"modifier_group_ids": [...]}`; array order is the per-product sort order.
- A staff member can't log in without a role, so the "no role → 403 on reads" E2E check revokes a cashier's only role after login.
- "Price change and revert within 24 h" (§7) is left for an owner report derived from `audit_logs`.
- Out of scope: per-outlet overrides, sold-out, public menu, POS bundle, caching (Outlet Catalog); images; combos; multi-axis variants; modifier quantities/nesting/defaults; day-of-week/seasonal schedules; i18n; CSV import/export; stock/recipes; POS price override, discounts, tax.

## History

<!-- Keep this updated from earliest to latest -->

- **2026-10-13** — Initial setup: HTTP skeleton (chi), typed env config, Postgres + Redis connections, `/health` and `/ready` endpoints.
- **2026-10-13** — Added project context docs (`CLAUDE.md`, `context/`): project spec, coding standards, AI interaction guidelines, and this feature tracker.
- **2026-10-16** — Auth slice 1 (`feature/auth-otp-sessions`): migration `0001_auth` with role seed, phone/email identifiers, OTP request/verify with cooldown, quotas, decoys and attempt limits, ES256 access tokens + JWKS, rotating refresh sessions with reuse detection and grace window, logout, revoke-all, audience middleware, log-stub notifier. Dev DB reset to this repo's migrations; coding standards updated to pgx; golangci-lint clean. Deviations: `device_enroll` deferred, revoke-all shared by customer/staff, full identifier in `auth_events`, logout revokes whole family.
- **2026-10-17** — Auth slice 2 (`feature/authz`): outlet-scoped authorization (`internal/authz` resolver with Redis cache at `authz:staff:{id}`, `AUTHZ_CACHE_TTL`, Postgres fallback; `LoadStaffAccess` / `RequireOutletPermission` middleware — no role at outlet → 404, missing permission → 403, deactivated → 403). Staff endpoints (`internal/staff`): list/create outlet staff, grant/revoke outlet and global roles, deactivate (revokes sessions + cache), admin session revoke. FR-AZ-6 escalation rules checked by role code. Migration `0002_authz` adds `audit_logs`; role and deactivation changes audited in the same transaction, which also serves as role-grant history. Verified: unit tests, AC-26 matrix test, authz E2E 41/41, slice 1 regression 39/39, golangci-lint clean. Out of scope: FR-AZ-7 (refunds), FR-AZ-9/10 (orders), FR-AUD-3 DB grants, full staff CRUD. Remaining auth slices: devices + PIN + roster → customer identifiers → step-up server side.
- **2026-10-19** — Auth slice 3 (`feature/devices-pin-roster`): staff PIN set/reset (`POST /v1/staff/me/pin`, `DELETE /v1/staff/{id}/pin`; 6 digits, weak-PIN rules + `PIN_DENYLIST`, argon2id PHC hash with `PIN_ARGON2_*` params, ~17 ms on the dev machine; `pin.set`/`pin.reset` in `auth_events`, reset actor in `audit_logs`). `device_enroll` OTP purpose gated on `device.enroll` at the target outlet; `POST /v1/auth/devices/enroll` redeems the code, enforces `DEVICE_LIMIT_PER_OUTLET` (409 `device_limit_reached`, or `replace` revokes the oldest in the same transaction; the code survives a 409), re-enrolls a known fingerprint under its original `device_id`, and opens a device-bound `pos` session (subject = device). `POST /v1/auth/devices/{id}/revoke` (`device.revoke`); Redis `refresh:device_revoked:{hash}` markers make the next refresh return 401 `device_revoked`, and refresh plus every `/v1/pos` route also check `devices.is_active` in Postgres. `last_seen_at` updated on refresh. `internal/pos`: `GET /v1/pos/roster` (outlet staff + global role holders, PIN hashes, permissions, content-hash version) and `POST /v1/pos/pin-failures` (client-generated IDs, idempotent). `RequireActingStaff` middleware for `X-Acting-Staff-Id`, not mounted yet. No migration. Verified: unit tests, AC-26 matrix test, slice 3 E2E 81/81, slice 1 smoke 8/8, golangci-lint clean. Out of scope: server-side step-up (FR-STEP-*), `/v1/sync/batch` and `sync.after_reenroll`, `authz.offline_violation`, device listing, POS-side encryption (NFR-SEC-7). Remaining auth slices: customer identifiers → step-up server side.
- **2026-10-20** — Auth slice 4 (`feature/customer-identifiers`): signed-in customers add or change their phone or email (FR-CUS-2/3). New OTP purpose `customer_identifier`, redeemed only by the action endpoint (never `/otp/verify`, never a session); its HMAC also binds the requesting customer ID so no other customer can redeem it. `POST /v1/me/identifiers/otp` sends the code (usual cooldown + quotas, standard 202 body, no email fallback), `POST /v1/me/identifiers` redeems it and sets `customers.phone`/`email` under a row lock. Ownership checked only at redeem time: `409 identifier_in_use` (new error code), no merge; setting the current value is a no-op. `audit_logs` row `customer.identifier_changed` (actor_type customer, before/after) in the same transaction; OTP events in `auth_events`. Existing sessions stay valid; staff phones don't count as in use (FR-CUS-4). No migration. Verified: unit tests, slice 4 E2E 33/33, device-enroll regression smoke, golangci-lint clean. Out of scope: guest-order linking (FR-CUS-1, AC-9) until orders exist. Remaining auth slices: step-up server side (with orders/sync).
