# Outlet Management — Feature Spec

## 1. Introduction

Outlets exist in the schema since `0001_auth` and every operational table points at them, but nothing in the API creates or changes one: outlets are inserted by hand. This feature lets an owner create, edit, deactivate and reactivate outlets from the dashboard, and lets staff read the outlets they work at.

A new outlet is usable at once: the owner's global role reaches it, the catalog applies with no overrides (absence = inherit), and the first shift open creates its order sequences.

**Depends on:**

- outlet-scoped authorization (`internal/authz`), global roles and `RequireGlobalPermission` / `RequireOutletRole`
- the `outlet.manage` permission, already seeded on the owner role (`0001_auth`), with no Go constant yet
- Outlet Catalog: the per-outlet menu and bundle cache (`docCache.invalidateOutlet`), the public outlet list
- Shifts Core: one open shift per outlet, business date fixed at open from `timezone` and `business_day_cutoff`

### 1.1 Goals

- An owner can bring a new outlet online without touching the database.
- Changing an outlet never corrupts a running business day: no shift, order number or sold-out expiry changes meaning halfway through.
- A closed outlet disappears from the online shop and its POS stops working, without losing any history.
- Every outlet change leaves an attributable trail.

### 1.2 In scope

- Create, read, list, update outlets
- Deactivate and reactivate (no delete)
- Validation of timezone, business-day cutoff and order prefix
- Cache invalidation for the outlet's menu and POS bundle
- Audit, error contract, tests

### 1.3 Out of scope

- **Opening hours** (`outlet_hours` in `context/spec/data-model.md`): not migrated; needed first by Web Orders ("orders outside operating hours queue to the next shift").
- Device listing and management screens beyond what exists (`/v1/auth/devices/...`)
- Per-outlet tax, service charge and packaging fee configuration
- Outlet images, geolocation, delivery zones
- Hard delete. Outlets are referenced by orders, shifts, payments and audit rows; they are deactivated, never removed.
- Custom roles or delegating `outlet.manage` to managers

### 1.4 Decisions carried from the project overview, earlier specs and the user

| Decision | Source |
| --- | --- |
| Multi-outlet from day one; every operational table carries `outlet_id` | Overview, Outlets |
| One POS device per outlet; order prefix is a flat `POS`, the prefix column allows `POS1`/`POS2` later | Overview, Terminals |
| Business day ≠ calendar day: per-outlet cutoff, timestamps in UTC, rendered in outlet timezone | Overview |
| A shift's business date is fixed at open; the shift's counter prefix is the one its sequences were created with | Shifts Core, Orders Core |
| `outlet.manage` belongs to the owner role only | `0001_auth` role seed |
| Catalog entities are archived, never deleted; same rule here | Catalog Core |
| Dashboard-created entities get a server-generated ID (no offline need) | Catalog Core |

### 1.5 Vocabulary

- **Active outlet**: `is_active = true`. Only active outlets appear publicly, take shifts, take orders or enroll devices (already enforced by the existing queries).
- **Day settings**: `timezone` and `business_day_cutoff`, which together decide an order's business date and when "today" ends for sold-out.

## 2. Actors and Access

| Actor | Surface | Action | Auth | Permission |
| --- | --- | --- | --- | --- |
| Owner | Dashboard | Create, update, deactivate, reactivate | `staff` | `outlet.manage` through a **global** role |
| Any staff | Dashboard | Read an outlet they have a role at | `staff` | any role reaching the outlet (else 404) |
| Any staff | Dashboard | List the outlets they can reach | `staff` | none beyond sign-in |
| Public | Online shop | List active outlets, read a menu | none | unchanged (`GET /v1/outlets`) |

`outlet.manage` is checked with `RequireGlobalPermission`, like `catalog.manage`: an outlet role cannot create outlets, and holding it at one outlet must not let someone edit another.

## 3. Functional Requirements

### 3.1 Fields

| Field | Rule |
| --- | --- |
| `name` | Required on create. Trimmed, 1–100 characters. |
| `address` | Optional, ≤ 500 characters, trimmed; empty string or `null` clears it. |
| `phone` | Optional, ≤ 30 characters, trimmed; `null` clears it. Free text (a landline is fine), not an identifier. |
| `timezone` | IANA name accepted by `time.LoadLocation` and by Postgres (`AT TIME ZONE`, checked in the write transaction, since reads use it); `""`, `Local` and `UTC`-offset forms like `+07:00` are refused. Default `Asia/Jakarta`. |
| `business_day_cutoff` | `HH:MM`, 24-hour, `00:00`–`23:59`. Default `04:00`. |
| `order_prefix` | 1–8 characters, `A–Z` and `0–9`, upper-cased on input. `WEB` is reserved and refused. Default `POS`. |
| `is_active` | Not writable through create or PATCH; changed only by the deactivate and activate routes. |

Unknown fields are refused (`httpx.DecodeValid` behaviour), so a typo never silently does nothing.

### 3.2 Create

- **FR-OTL-1** `POST /v1/outlets` creates an active outlet with a server-generated ID and returns `201` with the outlet.
- **FR-OTL-2** Omitted optional fields take the defaults in §3.1.
- **FR-OTL-3** Nothing else is created: no channel settings (absence = inherit), no sequences (created at shift open), no roles. Global-role staff reach the outlet at once; the authz cache needs no invalidation because global roles do not list outlets.

### 3.3 Read

- **FR-OTL-4** `GET /v1/outlets/{outletID}` returns every column, including `is_active`, `created_at`, `updated_at`, and whether a shift is open (`open_shift_id` or `null`), so the dashboard can explain why day settings are locked.
- **FR-OTL-5** `GET /v1/staff/me/outlets` lists the outlets the caller can reach, ordered by `lower(name), id`: every outlet for a global role, otherwise those with an outlet role. Inactive outlets are included with `is_active: false` only for callers with `outlet.manage`; others see active outlets only. No pagination (outlet counts are small); a hard cap of 500.
- **FR-OTL-6** The public `GET /v1/outlets` is unchanged. A dashboard list lives elsewhere because one route cannot be both public and authenticated.

### 3.4 Update

- **FR-OTL-7** `PATCH /v1/outlets/{outletID}` changes any of the §3.1 writable fields. Absent fields keep their value; `null` clears a nullable field (`address`, `phone`) and is refused on the rest.
- **FR-OTL-8** The update locks the outlet row (`FOR UPDATE`). If nothing changes after normalisation, it returns `200` with the outlet and writes no audit row and no `updated_at` change.
- **FR-OTL-9** **Day settings are locked while a shift is open.** A change to `timezone` or `business_day_cutoff` while the outlet has an open shift is refused with `409 outlet_shift_open` and `details.shift_id`. Reason: the open shift's business date is already fixed, but sold-out "today" expiry, the dashboard's default "today" and the next open all read the live values, so a mid-day change would split one business day across two definitions.
- **FR-OTL-10** `order_prefix` may change while a shift is open. It takes effect at the next shift open; the open shift keeps numbering with the prefix its sequences were created with (Orders Core already guarantees this). The response says nothing extra; the dashboard copy explains it.
- **FR-OTL-11** `name`, `address` and `phone` can change at any time.
- **FR-OTL-12** After a committed change to `name`, `timezone` or `business_day_cutoff`, the outlet's menu and bundle cache generation is bumped (`invalidateOutlet`), since both documents embed them. A failed bump is logged and does not fail the request (existing `bumpTimeout` behaviour).

### 3.5 Deactivate and reactivate

- **FR-OTL-13** `POST /v1/outlets/{outletID}/deactivate` sets `is_active = false`. Refused with `409 outlet_shift_open` while a shift is open: the shift must be closed first, so no shift is stranded at an outlet that can no longer take orders.
- **FR-OTL-14** In the same transaction, every active device at the outlet is marked revoked (`revoked_by` = the caller). After commit, each device's sessions are revoked through the same `Revoker.RevokeDevice` path `POST /v1/auth/devices/{id}/revoke` uses, with its `session.revoked` auth event. The POS stops on its next request with `device_revoked` (`RequireActiveDevice` reads the device row, so this holds even if the session revocation fails; a failure is logged, not returned).
- **FR-OTL-15** Staff roles at the outlet are kept, so reactivation needs no re-granting. Inactive outlets are already refused by shift open, Quote, device enroll, menu and sold-out (existing `AND is_active` queries); nothing new is needed there.
- **FR-OTL-16** `POST /v1/outlets/{outletID}/activate` sets `is_active = true`. Devices are not restored; the outlet enrolls a device again.
- **FR-OTL-17** Both are idempotent: deactivating an inactive outlet, or activating an active one, returns `200` with `applied: false`, no audit row and no device change.
- **FR-OTL-18** Both bump the outlet's cache generation after commit.

### 3.6 Concurrency

- **FR-OTL-19** Shift open reads the outlet `FOR SHARE` (today a plain read). Together with FR-OTL-8's `FOR UPDATE`, this serialises a shift open against a day-settings change or a deactivation: either the open waits and sees the new values, or the change waits and sees the open shift and is refused.

### 3.7 Authorization summary

| Route | Middleware |
| --- | --- |
| `POST /v1/outlets` | `staff` + `LoadStaffAccess` + `RequireGlobalPermission(outlet.manage)` |
| `PATCH /v1/outlets/{id}`, `POST …/deactivate`, `POST …/activate` | same |
| `GET /v1/outlets/{id}` | `staff` + `LoadStaffAccess` + `RequireOutletRole` (404 without a role) |
| `GET /v1/staff/me/outlets` | `staff` + `LoadStaffAccess` (in the existing `/v1/staff` route group) |

An unknown outlet ID is `404` for everyone. A caller without `outlet.manage` gets `403` on write routes for an outlet they can reach, and `404` for one they cannot (FR-AZ-5 rule: do not reveal existence). Bodies are capped at 4 KB.

### 3.8 Audit

One `audit_logs` row per applied change, `entity_type = 'outlet'`, `outlet_id` = the outlet, `actor_type = 'staff'`:

| Action | `before` / `after` |
| --- | --- |
| `outlet.created` | `after`: all fields |
| `outlet.updated` | changed fields only, both sides |
| `outlet.deactivated` | `after`: `{"is_active": false, "devices_revoked": n}` |
| `outlet.activated` | `after`: `{"is_active": true}` |

Each device revoked by FR-OTL-14 gets the same `device.revoked` audit row `POST /v1/auth/devices/{id}/revoke` writes, in the same transaction, and a `session.revoked` auth event after commit; `devices_revoked` on the outlet row records the count.

## 4. Non-Functional Requirements

- **Security:** only global `outlet.manage` writes; no outlet field is secret, but the staff list only shows outlets the caller can reach. Input is length-capped and the body capped at 4 KB.
- **Performance:** every route is a handful of single-row queries; p95 < 50 ms locally. The staff list is one query.
- **Resilience:** Postgres unreachable → `503` with the existing internal error mapping; Redis down only skips the cache bump.
- **Testability:** integration tests on `madkunyah_test` (`TEST_DATABASE_URL`), route walk on the real router, unit tests for field validation.

## 5. Edge Cases and Error Handling

### 5.1 Error contract

| Status | Code | When |
| --- | --- | --- |
| 400 | `invalid_input` | field rules in §3.1, unknown field, `null` on a non-nullable field |
| 403 | `forbidden` | write without global `outlet.manage` on a reachable outlet |
| 404 | `not_found` | unknown outlet, or no role reaching it |
| 409 | `outlet_shift_open` (new) | day-settings change or deactivate while a shift is open; `details.shift_id` |
| 413 | `payload_too_large` | body > 4 KB |

### 5.2 Edge cases

- **E-1** PATCH with `{}` → `200`, no audit.
- **E-2** PATCH `order_prefix: "pos"` on an outlet whose prefix is `POS` → normalised, no change, no audit.
- **E-3** PATCH `timezone` to the value it already has while a shift is open → no change, so not refused.
- **E-4** PATCH `name` and `timezone` together while a shift is open → whole request refused; nothing changes.
- **E-5** Deactivate with an open shift that has unfinished orders → `409 outlet_shift_open`; the cashier closes the shift, which already requires the orders to be finished.
- **E-6** Deactivate races a shift open → serialised by FR-OTL-19; exactly one succeeds.
- **E-7** A POS that cached the bundle of a now-inactive outlet → its next authenticated call gets `device_revoked`.
- **E-8** Two outlets with the same name → allowed (see open questions); names are labels, IDs identify.
- **E-9** `order_prefix: "WEB"` → `400`; `WEB` is the web sequence's prefix.

## 6. Acceptance Criteria

- **AC-1** Owner creates an outlet with only `name`; it has the defaults, is active, appears in `GET /v1/outlets` and the owner's `GET /v1/staff/me/outlets`, and one `outlet.created` audit row exists.
- **AC-2** A manager (outlet role with no `outlet.manage`) gets `403` on create and on PATCH of their own outlet, and `404` on PATCH of an outlet they have no role at.
- **AC-3** Each invalid field in §3.1 returns `400` naming the field; nothing is written.
- **AC-4** PATCH of `name`, `address`, `phone` applies, writes one audit row with only the changed fields, and bumps the menu cache (the next menu read shows the new name).
- **AC-5** With a shift open, PATCH `timezone` or `business_day_cutoff` returns `409 outlet_shift_open` with `details.shift_id`; after the shift closes the same request applies.
- **AC-6** With a shift open, PATCH `order_prefix` applies; the next order in that shift keeps the old prefix and the next shift uses the new one.
- **AC-7** Deactivate with a shift open → `409`; after close → `200`, the outlet is gone from the public list, the menu is `404`, its device's next POS call is `device_revoked`, staff roles remain.
- **AC-8** Activate restores the public listing; the old device stays revoked and a new enrolment works.
- **AC-9** Repeated deactivate/activate returns `applied: false` with no audit row.
- **AC-10** Concurrent shift open and deactivate: exactly one succeeds (race detector, repeated).
- **AC-11** `GET /v1/outlets/{id}` returns `404` to staff without a role there; a cashier at the outlet can read it, including `open_shift_id`.
- **AC-12** The route walk lists the five new routes with the middleware in §3.7.

## 7. Data Model — Migration `0009_outlet_management`

No new tables. Constraints so the database agrees with §3.1 (existing rows all use `POS` / `Asia/Jakarta` / `04:00` and satisfy them):

```sql
ALTER TABLE outlets
  ADD CONSTRAINT outlets_name_not_blank CHECK (btrim(name) <> '' AND length(name) <= 100),
  ADD CONSTRAINT outlets_order_prefix_format CHECK (order_prefix ~ '^[A-Z0-9]{1,8}$' AND order_prefix <> 'WEB'),
  ADD CONSTRAINT outlets_address_length CHECK (length(address) <= 500),
  ADD CONSTRAINT outlets_phone_length CHECK (length(phone) <= 30);
```

The timezone has no CHECK (Postgres has no cheap IANA check in a constraint); the write transaction runs `now() AT TIME ZONE $1` instead, so Go and Postgres both accept every stored name.

Code: `domain.PermOutletManage = "outlet.manage"`, `domain.CodeOutletShiftOpen` (409 in `httpx`). A new `internal/outlet` package (handler, service, repository) owns the routes; the device revocation reuses the auth device repository's revoke query through a small interface, not a copy.

## 8. API

```
# Dashboard (staff audience)
POST   /v1/outlets                          outlet.manage (global)
GET    /v1/outlets/{outletID}               any role at the outlet
PATCH  /v1/outlets/{outletID}               outlet.manage (global)
POST   /v1/outlets/{outletID}/deactivate    outlet.manage (global)
POST   /v1/outlets/{outletID}/activate      outlet.manage (global)
GET    /v1/staff/me/outlets                 signed-in staff

# Public, unchanged
GET    /v1/outlets
GET    /v1/outlets/{outletID}/menu
```

Create body:

```json
{
  "name": "MadKunyah Kemang",
  "address": "Jl. Kemang Raya 10, Jakarta",
  "phone": "021-555-0101",
  "timezone": "Asia/Jakarta",
  "business_day_cutoff": "04:00",
  "order_prefix": "POS"
}
```

Outlet response:

```json
{
  "id": "uuid",
  "name": "MadKunyah Kemang",
  "address": "Jl. Kemang Raya 10, Jakarta",
  "phone": "021-555-0101",
  "timezone": "Asia/Jakarta",
  "business_day_cutoff": "04:00",
  "order_prefix": "POS",
  "is_active": true,
  "open_shift_id": null,
  "created_at": "2026-11-20T03:00:00Z",
  "updated_at": "2026-11-20T03:00:00Z"
}
```

Deactivate and activate return the outlet plus `"applied": true | false`.

## 9. Risks and Open Questions

| Item | Recommendation |
| --- | --- |
| **Unique outlet names?** Duplicates confuse the dashboard picker. | Not unique for now (dev data already has 12 "E2E outlet" rows). Revisit with a case-insensitive unique index among active outlets if it bites. |
| **Restrict timezones to Indonesia** (`Asia/Jakarta`, `Asia/Makassar`, `Asia/Jayapura`)? | Accept any valid IANA name; restricting is a one-line change later. |
| **Deactivation revokes devices** (FR-OTL-14) — stronger than strictly needed. | Keep: a closed outlet with a live POS token is a loose end, and re-enrolment is cheap. |
| **Should managers edit name/address/phone of their own outlet?** | No for now; owner only, matching the seed. A per-field permission (like FR-OUT-4) can come later. |
| `FOR SHARE` on shift open adds a lock to a hot path. | One row, held for the open transaction only; negligible at one device per outlet. |
