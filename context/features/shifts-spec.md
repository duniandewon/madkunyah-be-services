# Shifts Core — Feature Spec

## 1. Introduction

This feature gives every outlet a **shift**: the window of trading that order numbers, settlement, and the shift report are scoped to. It adds:

- opening and closing a shift from the POS, with supervisor approval when the cashier lacks the permission
- the shift's **business date**, fixed when the shift opens
- per-shift **order sequences**, and the internal interfaces the Orders feature uses to find the open shift and allocate order numbers
- the close guard: a shift with unfinished orders cannot close. The POS checks its own orders first, then the server checks again

Shifts are opened and closed **only on the POS**, never from the dashboard. Opening and closing **always require a connection to the server**. They are never queued offline. Selling during an open shift still works offline, as before.

**Depends on:**

- outlet-scoped authorization (`internal/authz`), including global roles
- POS devices, the `pos` audience, `RequireActiveDevice` and `RequireActingStaff` (auth slice 3)
- `outlets.timezone` and `outlets.business_day_cutoff` (migration `0001_auth`)
- `audit_logs`, including `approved_by` (migration `0002_authz`)

**Related:**

- **Orders** places orders into the open shift, allocates numbers through §3.4, and implements the close guard (§3.5).
- **Shift Settlement** (later) adds per-tender reconciliation to the close step, using `shift_tender_reconciliations`.
- **Reports** adds the shift report (`GET /v1/reports/shifts/{id}`, `shift.report.view`).

### 1.1 Goals

- Every order can be tied to exactly one shift, and its number is unique within that shift.
- At most one shift is open per outlet, enforced by the database.
- A retried open or close never creates a second shift or a second audit row.
- A shift cannot close while it still has orders in progress.
- Every open and close leaves an attributable trail: who acted, who approved, from which device.

### 1.2 In scope

- `shifts` and `order_sequences` tables
- POS: read the current shift, open, close
- Dashboard: list shifts, read one shift (read-only)
- Business-date calculation
- Internal interfaces for Orders: current shift lookup, number allocation, close guard
- Server-side check of the step-up approver for open and close
- Auditing, metrics, and error contract for the above

### 1.3 Out of scope

- **Settlement.** Per-tender expected vs actual, variance and notes at close → Shift Settlement. Until then, closing records no amounts.
- **Shift report** (sales, voids, discounts, refunds, per-employee) → Reports
- **Orders themselves**, including how web orders placed while no shift is open queue to the next shift → Orders
- **Opening or closing a shift offline**, or through `/v1/sync/batch`. Never supported (FR-SH-2).
- Reopening a closed shift. Never supported; open a new shift instead.
- Opening or closing a shift from the dashboard. Never supported (FR-SH-8).
- Scheduled or automatic close
- Multiple concurrent shifts per outlet (one per device, one per terminal). The prefix structure allows a second device later without a schema change, but the one-open-shift rule would have to change.

### 1.4 Decisions carried from the project overview and the user

| Decision | Source |
| --- | --- |
| No opening float, no drawer, no cash count | Overview §G, Scope Decisions |
| Order sequence resets at the start of each shift | Overview §E |
| Order number uniqueness is `(outlet_id, shift_id, prefix, sequence)` | Overview §E |
| Web orders attach to the outlet's currently open shift | Overview §E |
| Shift open/close needs `shift.open` / `shift.close`; step-up when the acting staff member lacks it | Auth spec FR-STEP-1 |
| **Opening and closing a shift require a connection, without exception.** Staff connect the device one way or another (outlet Wi-Fi, mobile hotspot) | Decided for this feature |
| **Until settlement exists, unfinished orders block closing a shift**, whether they are on the server or still on the POS | Decided for this feature |
| **Shifts are opened and closed only on the POS.** The dashboard cannot close a shift | Decided for this feature |
| **Before closing, the POS checks for ongoing orders first, then for a connection** | Decided for this feature |

### 1.5 Vocabulary

| Term | Meaning |
| --- | --- |
| Shift | A period of trading at one outlet, from open to close. `status ∈ open \| closed` |
| Current shift | The outlet's one open shift, if any |
| Business date | The trading day a shift belongs to. Fixed at open (FR-SH-6) |
| Prefix | The order-number prefix: the outlet's `order_prefix` (today `POS`) for counter orders, `WEB` for online orders |
| Unfinished order | An order whose status is not terminal. Defined by Orders; see FR-CLS-3 |
| Acting staff | The staff member operating the POS, from `X-Acting-Staff-Id` |
| Approver | A staff member who holds the permission and approves the action for an acting staff member who doesn't (step-up) |

---

## 2. Actors and Access

| Actor | Client | What they do | Audience | Permission |
| --- | --- | --- | --- | --- |
| Supervisor, manager, owner | POS | Open and close a shift | `pos` + acting staff | `shift.open`, `shift.close` (outlet) |
| Cashier | POS | Open or close a shift **with an approver** present | `pos` + acting staff | approver holds `shift.open` / `shift.close` |
| Any outlet staff | POS | See whether a shift is open | `pos` + acting staff | any role at the outlet |
| Any staff with a role at the outlet | Dashboard | List and view shifts | `staff` | any role at the outlet |

No new permission strings are needed. `shift.open`, `shift.close` and `shift.report.view` are already seeded (migration `0001_auth`).

---

## 3. Functional Requirements

Requirement IDs are stable and referenced by the acceptance criteria in §6.

### 3.1 Lifecycle

- **FR-SH-1** A shift belongs to one outlet and moves `open → closed`, once. A closed shift never reopens.
- **FR-SH-2** **Online only.** Open and close are synchronous server calls. The server is the only source of a shift's existence and status. The POS:
  - disables open and close while offline, and tells staff to connect the device
  - never queues an open or close, and never fabricates a local shift
  - cannot create orders until it has received an open shift from the server

  `/v1/sync/batch`, when built, rejects any shift open or close operation.
- **FR-SH-3** **One open shift per outlet**, enforced by the partial unique index `one_open_shift_per_outlet`. The server returns `409 shift_already_open` when it is violated, including when two requests race.
- **FR-SH-4** All shift timestamps (`opened_at`, `closed_at`) are **server time**. The request carries no client time, because the device is online.
- **FR-SH-5** A shift can be opened only at an active outlet, from an active device enrolled at that outlet.
- **FR-SH-6** **Business date.** At open, `business_date` is the outlet-local date of `opened_at − business_day_cutoff`, in the outlet's timezone. With a 04:00 cutoff:
  - opened 2026-11-12 09:00 local → `2026-11-12`
  - opened 2026-11-12 02:30 local → `2026-11-11`

  It never changes afterwards, even if the shift stays open past the next cutoff. An outlet may have several shifts on one business date (for example morning and evening).
- **FR-SH-7** A shift records the device that opened it (`device_id`) and the device that closed it (`closed_device_id`). Closing from a different device at the same outlet is allowed, so a replacement device can close a shift its predecessor opened.
- **FR-SH-8** **POS only.** There is no dashboard route that opens or closes a shift. If the POS that opened a shift is lost or broken, a manager enrolls a replacement device at the outlet (auth slice 3), and that device closes the shift under FR-SH-7.

### 3.2 Opening (POS)

- **FR-OPN-1** `POST /v1/pos/shifts` with `X-Acting-Staff-Id` and body:

  ```json
  { "id": "<client-generated UUID>", "approved_by": "<staff UUID, optional>" }
  ```

  The outlet and device come from the device token.
- **FR-OPN-2** **Idempotent by `id`.** The client generates the shift ID once per open attempt and reuses it on every retry:

  | Existing state | Response |
  | --- | --- |
  | No shift with this `id`, no open shift at the outlet | `201`, shift created, `applied: true` |
  | This `id` is already the outlet's open shift | `200`, `applied: false`, current shift returned, no audit row |
  | Another shift is open at the outlet | `409 shift_already_open`, `details` holds the open shift's `id`, `opened_at`, `opened_by`, `business_date` |
  | This `id` exists and is closed | `409 shift_closed` |
  | This `id` exists at another outlet | `409 conflict` |

- **FR-OPN-3** In the same transaction as the insert, one `order_sequences` row is created per prefix — the outlet's `order_prefix` and `WEB` — with `last_value = 0`. This is the per-shift reset.
- **FR-OPN-4** The response returns the shift (§3.6 shape).

### 3.3 Step-up for open and close

- **FR-STP-1** If the acting staff member holds the permission (`shift.open` or `shift.close`) at the device's outlet, the action proceeds. `approved_by` is ignored, and the audit row records the actor as approver (auth spec FR-STEP-5).
- **FR-STP-2** If the acting staff member lacks the permission, `approved_by` is required. The server checks, through server-side authz at request time, that the approver is active and holds the permission at the outlet. The POS has already checked the approver's PIN against its roster (auth spec FR-STEP-2).
- **FR-STP-3** Errors:
  - acting staff lacks the permission and `approved_by` is absent → `403 forbidden`
  - approver unknown, deactivated, without a role at the outlet, or without the permission → `403 forbidden`
- **FR-STP-4** This feature is the first to check `approved_by` server side. The check is a shared helper, `authz.Approver`, so Orders can reuse it for voids, discounts and price overrides.

### 3.4 Order sequences — internal interface for Orders

- **FR-SEQ-1** `shift.Current(ctx, q, outletID)` returns the outlet's open shift, locking its row `FOR SHARE` in the caller's transaction, or `ErrNoOpenShift`. Orders call it when placing an order. The shared lock makes a concurrent close wait until the placement commits (FR-CLS-4).
- **FR-SEQ-2** `shift.NextNumber(ctx, q, shiftID, prefix)` allocates the next number with a single
  `UPDATE order_sequences SET last_value = last_value + 1 WHERE shift_id = $1 AND prefix = $2 RETURNING last_value`
  in the caller's transaction, and returns the sequence and the display string.
- **FR-SEQ-3** **Format:** `{prefix}-{sequence}`, with the sequence zero-padded to at least 3 digits: `POS-001`, `WEB-042`, `POS-1000`.
- **FR-SEQ-4** An unknown `(shift_id, prefix)` returns `ErrUnknownSequence`. `NextNumber` never creates sequence rows; FR-OPN-3 does.
- **FR-SEQ-5** `NextNumber` does not deduplicate retries. Orders gets "a retry returns the same number" by storing the number on the order row, keyed by the client order UUID, and allocating only on first insert (overview §E). If the transaction rolls back, the increment rolls back with it, so no number is burned.
- **FR-SEQ-6** The caller must hold the lock from FR-SEQ-1 on the same shift. `NextNumber` on a closed shift returns `ErrShiftClosed`.

### 3.5 Closing

- **FR-CLS-1** POS: `POST /v1/pos/shifts/{shift_id}/close` with `X-Acting-Staff-Id` and body `{ "approved_by": "…" }` (optional, FR-STP-2). The shift must belong to the device's outlet, or the request returns `404 not_found`.
- **FR-CLS-2** **POS pre-close checks** (client contract, documented for the POS team). When staff tap *Close shift*, the POS runs these checks **in this order** and stops at the first that fails:
  1. **Ongoing orders on the device.** Any local order for this shift that is unfinished (FR-CLS-3), including open tickets not yet submitted and orders created offline. If there are any, the POS refuses and lists them so staff can finish or cancel them. This check needs no connection, so staff resolve orders before being asked to connect.
  2. **Connection.** If the device is offline, the POS refuses and tells staff to connect it (FR-SH-2).
  3. **Sync.** The POS sends every queued offline operation for this shift and waits until all are accepted. If any fails, it does not close.
  4. **Close call.** Only then does the POS call the close route.

  The server cannot see the device's local state, so it does not trust these checks. It runs its own guard (FR-CLS-3) on every close.
- **FR-CLS-3** **Unfinished orders block close.** Until settlement exists, a shift with any unfinished order cannot close:
  - The close returns `409 shift_has_unfinished_orders`. `details.orders` lists up to 50 of them, each with `id`, `order_number`, `status`, and `channel`, and `details.count` holds the total.
  - **Unfinished** means any status other than `completed`, `cancelled`, `expired` or `refunded`: today `draft`, `pending_payment`, `placed`, `preparing`, `ready`. Orders owns this list and must keep it current when it changes the state machine.
  - The check goes through an `OrderGuard` interface (`UnfinishedOrders(ctx, q, shiftID, limit) ([]OrderRef, int, error)`: up to `limit` orders, and the total). This slice ships a stub that always returns none, because the `orders` table does not exist yet. **Orders replaces the stub** and adds the acceptance test (AC-11).
- **FR-CLS-4** **No race with order placement.** Close locks the shift row `FOR UPDATE`, then runs the guard, then sets `status = closed`, all in one transaction. A placement holding `FOR SHARE` (FR-SEQ-1) either commits first, so the guard sees its order, or waits and then sees the shift closed.
- **FR-CLS-5** **Idempotent.** Closing an already-closed shift returns `200`, `applied: false`, the shift as stored, and writes no audit row.
- **FR-CLS-6** The close sets `closed_at` (server time), `closed_by` (the acting staff member), and `closed_device_id` (the calling device).
- **FR-CLS-7** On a `409 shift_has_unfinished_orders`, the POS shows the server's list so staff can finish or cancel those orders, then runs FR-CLS-2 again from step 1.

### 3.6 Reads

- **FR-RD-1** `GET /v1/pos/shifts/current` returns `200 { "shift": {…} }` with the outlet's open shift, or `200 { "shift": null }`. The POS calls it at start-up and on reconnect.
- **FR-RD-2** `GET /v1/outlets/{outlet_id}/shifts?from=&to=&status=` lists shifts by `business_date` range (inclusive, both optional, at most 92 days), newest first, cursor-paginated (default 50, max 200).
- **FR-RD-3** `GET /v1/outlets/{outlet_id}/shifts/{shift_id}` returns one shift.
- **FR-RD-4** **Shape:**

  ```json
  {
    "id": "…", "outlet_id": "…", "status": "open",
    "business_date": "2026-11-12",
    "opened_at": "2026-11-12T02:01:00Z", "opened_by": { "id": "…", "name": "Rina" },
    "device_id": "…",
    "closed_at": null, "closed_by": null, "closed_device_id": null,
    "sequences": [ { "prefix": "POS", "last_value": 42 }, { "prefix": "WEB", "last_value": 7 } ]
  }
  ```

  `sequences.last_value` is the count of numbers allocated, not a sales figure. Sales belong to the shift report.

### 3.7 Authorization summary

| Route | Audience | Requirement | No role at outlet |
| --- | --- | --- | --- |
| `GET /v1/pos/shifts/current` | `pos` + acting staff | any role at the device's outlet | `403` |
| `POST /v1/pos/shifts` | `pos` + acting staff | `shift.open`, or an approver who holds it (FR-STP) | `403` |
| `POST /v1/pos/shifts/{shift_id}/close` | `pos` + acting staff | `shift.close`, or an approver who holds it | `403` |
| `GET /v1/outlets/{outlet_id}/shifts`, `GET …/shifts/{shift_id}` | `staff` | any role at the outlet | `404` |

FR-AZ-5 applies to every outlet-scoped `staff` route. Global role holders are treated as having a role at every outlet. Every POS route also runs `RequireActiveDevice`.

### 3.8 Audit

- **FR-SAUD-1** Open and close each write one `audit_logs` row in the **same transaction**:
  - `outlet_id` set, `entity_type = shift`, `entity_id` = the shift ID
  - `actor_type = staff`, `actor_id` = the acting staff member
  - `approved_by` = the approver, or the actor when no step-up was needed
- **FR-SAUD-2** Actions and `after` contents:

  | Action | `after` |
  | --- | --- |
  | `shift.opened` | `business_date`, `device_id` |
  | `shift.closed` | `closed_device_id`, `sequences` (last value per prefix) |
- **FR-SAUD-3** No-op retries (FR-OPN-2, FR-CLS-5) and rejected requests write no audit row.

---

## 4. Non-Functional Requirements

### 4.1 Security

- **NFR-SEC-1** The approver check (FR-STP-2) uses server-side authz at request time, never the roster the POS sent.
- **NFR-SEC-2** POS routes are scoped to the device's outlet. A shift ID from another outlet behaves as unknown (`404`).
- **NFR-SEC-3** JSON request bodies are capped at 4 KB.

### 4.2 Performance

- **NFR-PERF-1** Open and close: p95 ≤ 100 ms, excluding the guard's query once Orders plugs it in.
- **NFR-PERF-2** `Current` + `NextNumber` together add p95 ≤ 5 ms to an order placement.
- **NFR-PERF-3** `GET /v1/pos/shifts/current`: p95 ≤ 30 ms.

### 4.3 Availability and resilience

- **NFR-AV-1** This feature uses no Redis of its own. Authz falls back to Postgres as it already does.

  | Failure | Behaviour |
  | --- | --- |
  | Postgres down | `503 shift_unavailable` |
  | Commit outcome uncertain (`postgres.ErrCommitUncertain`) | `503 shift_unavailable`. The client retries with the same `id`, and FR-OPN-2 / FR-CLS-5 make the retry safe |
  | Device offline at open or close | The POS refuses locally (FR-SH-2). Nothing reaches the server |

- **NFR-AV-2** Selling during an open shift does not depend on the server. A POS that loses its connection mid-shift keeps selling and syncs later.

### 4.4 Operability and testability

- **NFR-OPS-1** The clock is injectable, so business-date tests are deterministic, including just before and just after the cutoff and across midnight.
- **NFR-OPS-2** Business-date calculation has a table-driven unit test, with a non-default timezone and cutoff.
- **NFR-OPS-3** Integration tests run against `TEST_DATABASE_URL`, as in the catalog features.
- **NFR-OPS-4** The OpenAPI spec documents every route and error code in §5, once OpenAPI exists (deferred project-wide).
- **NFR-OPS-5** The metrics in §7, once Prometheus is wired in (deferred project-wide; see §7).

---

## 5. Edge Cases and Error Handling

### 5.1 Error contract

Uses the existing envelope. Codes used here:

| HTTP | `code` | When | Notes |
| --- | --- | --- | --- |
| 400 | `invalid_input` | Bad UUID, bad date range | |
| 401 | `device_revoked` | POS route from a revoked device | Existing device check |
| 403 | `forbidden` | Acting staff and approver both lack the permission; approver invalid; acting staff has no role at the outlet | |
| 404 | `not_found` | Unknown shift, shift at another outlet, inactive outlet, or no role at the outlet (dashboard) | FR-AZ-5 |
| 409 | `shift_already_open` | **New.** Another shift is open at the outlet | `details`: the open shift |
| 409 | `shift_closed` | Existing code. Open retried with the `id` of a closed shift | |
| 409 | `shift_has_unfinished_orders` | **New.** Close blocked by FR-CLS-3 | `details`: `orders`, `count` |
| 409 | `conflict` | Open with an `id` that belongs to another outlet | |
| 413 | `payload_too_large` | Body over 4 KB | |
| 503 | `shift_unavailable` | **New.** Postgres down or commit uncertain | |

### 5.2 Edge cases

**Opening**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-1 | Supervisor opens at 08:00 with a 04:00 cutoff | `business_date` is today |
| E-2 | Shift opened at 02:30, before the 04:00 cutoff | `business_date` is yesterday |
| E-3 | Shift opened 18:00, still open at 05:00 the next day | `business_date` stays the opening day. Orders after the cutoff still belong to this shift |
| E-4 | Network drops after the server commits the open; POS retries with the same `id` | `200`, `applied: false`, same shift, one audit row |
| E-5 | POS retries with a **new** `id` after a commit it never saw | `409 shift_already_open`, `details` names the shift that was opened. The POS adopts it |
| E-6 | Cashier opens, no `approved_by` | `403 forbidden` |
| E-7 | Cashier opens with a supervisor as `approved_by` | `201`. Audit: actor = cashier, `approved_by` = supervisor |
| E-8 | Approver was demoted after the POS last fetched the roster | `403 forbidden`. The server's view wins (NFR-SEC-1) |
| E-9 | Device is offline at opening time | POS refuses; staff connect via hotspot. No orders can be created until a shift is open (FR-SH-2) |
| E-10 | Outlet deactivated | `404 not_found` |
| E-11 | Two open requests race with different `id`s | One `201`, the other `409 shift_already_open` from the unique index |

**Closing**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-12 | Close while a counter order is `ready`, waiting for pickup | The POS refuses at step 1 (FR-CLS-2), without contacting the server. Staff hand it over (`completed`) or cancel it, then close again |
| E-13 | Close while an open ticket is still being entered | The POS refuses at step 1 and lists the ticket |
| E-14 | Close while offline, with no ongoing orders on the device | The POS refuses at step 2 and asks staff to connect |
| E-15 | Close while offline, **with** an ongoing order | The POS reports the ongoing order (step 1), not the missing connection |
| E-16 | Close while a web order is `pending_payment` | The POS can't see web orders, so it calls close; the server returns `409 shift_has_unfinished_orders`. Blocked until the webhook settles it or the QRIS expiry job expires it |
| E-17 | Orders queued offline fail to sync at step 3 | The POS does not call close. Staff retry once the sync succeeds |
| E-18 | Order placement and close race | Either the order commits first and blocks the close, or the close commits first and the placement sees `ErrNoOpenShift` / `ErrShiftClosed` (FR-CLS-4) |
| E-19 | Close retried after a lost response | `200`, `applied: false`, no second audit row |
| E-20 | POS device broken or lost with the shift open | A manager enrolls a replacement device at the outlet; it closes the shift (FR-SH-8). Orders that only existed on the lost device are lost with it |
| E-21 | POS closes a shift from another outlet by ID | `404 not_found` |
| E-22 | Someone calls a dashboard close route | No such route exists |

**Numbering**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-23 | First order of a new shift | `POS-001`, whatever the previous shift reached |
| E-24 | Order transaction rolls back after `NextNumber` | The increment rolls back; the next order gets the same number |
| E-25 | 1,000th POS order in one shift | `POS-1000` |
| E-26 | Web order arrives while no shift is open | `Current` returns `ErrNoOpenShift`. Orders decides how it queues (overview §E) |

---

## 6. Acceptance Criteria

Each criterion is an automated integration test unless marked *(manual)*.

### Lifecycle and opening

- **AC-1** *(FR-SH-6)* A table-driven test covers business date: before and after the cutoff, at exactly the cutoff, across midnight, in `Asia/Jakarta` and one other timezone, with a non-default cutoff.
- **AC-2** *(FR-OPN-1, FR-OPN-3)* A supervisor opens a shift: `201`, `status = open`, `device_id` set, and `order_sequences` rows for the outlet's prefix and `WEB`, both at 0.
- **AC-3** *(FR-OPN-2)* Retrying with the same `id` returns `200`, `applied: false`, and leaves exactly one shift and one audit row.
- **AC-4** *(FR-SH-3)* With a shift open, opening with a different `id` returns `409 shift_already_open` with the open shift in `details`. Two concurrent opens produce exactly one shift.
- **AC-5** *(FR-OPN-2)* Opening with the `id` of a closed shift returns `409 shift_closed`.

### Step-up

- **AC-6** *(FR-STP-1, FR-STP-3)* A cashier without `approved_by` gets `403` and nothing is written.
- **AC-7** *(FR-STP-2, FR-SAUD-1)* A cashier with a supervisor approver succeeds; the audit row has `actor_id` = cashier and `approved_by` = supervisor. A supervisor acting alone gets `approved_by` = themselves.
- **AC-8** *(FR-STP-3)* An approver who is a cashier, is deactivated, or has no role at the outlet → `403`.

### Numbering

- **AC-9** *(FR-SEQ-2, FR-SEQ-3)* Three `NextNumber` calls on a new shift return `POS-001`, `POS-002`, `POS-003`; a call for `WEB` returns `WEB-001`. A new shift starts again at `POS-001`.
- **AC-10** *(FR-SEQ-5)* A `NextNumber` inside a rolled-back transaction does not advance the sequence. Twenty concurrent allocations produce twenty distinct, gapless numbers.

### Closing

- **AC-11** *(FR-CLS-3)* **Owned by Orders.** With an order in each unfinished status, close returns `409 shift_has_unfinished_orders` listing it; once every order is terminal, close succeeds. In this slice, a test with a fake `OrderGuard` returning one order verifies the `409`, its `details`, and that nothing is written.
- **AC-12** *(FR-CLS-4)* With `Current` holding `FOR SHARE` in an open transaction, a concurrent close blocks until that transaction ends.
- **AC-13** *(FR-CLS-5)* Closing a closed shift returns `200`, `applied: false`, and writes no second audit row.
- **AC-14** *(FR-SH-8)* No `staff`-audience route opens or closes a shift: `POST /v1/outlets/{outlet_id}/shifts/{shift_id}/close` and `POST /v1/outlets/{outlet_id}/shifts` return `404` or `405`, and the shift is unchanged.
- **AC-15** *(FR-SH-7, FR-SH-8, FR-CLS-6)* A shift opened by device A can be closed from device B enrolled at the same outlet; `closed_by` = the acting staff member and `closed_device_id` = B.

### Reads and access

- **AC-16** *(FR-RD-1)* `GET /v1/pos/shifts/current` returns the open shift, and `{ "shift": null }` after it closes.
- **AC-17** *(FR-RD-2)* The list filters by business-date range and status, and rejects a range over 92 days.
- **AC-18** *(NFR-SEC-2)* A POS request for another outlet's shift returns `404`. A revoked device gets `401 device_revoked`.
- **AC-19** *(manual, FR-SH-2)* On the POS, with networking disabled, open is refused and the app asks staff to connect.
- **AC-20** *(manual, FR-CLS-2)* On the POS, with networking disabled and an open ticket, *Close shift* reports the ongoing ticket, not the missing connection. After the ticket is completed, it asks staff to connect. Once online, it syncs the queue and then closes.

---

## 7. Metrics

Prometheus is not wired in yet; both catalog features deferred it. These are the metrics to emit once it is. Until then, the signals in §7.2 can be read from `shifts` and `audit_logs`.

### 7.1 Health and experience

| Metric | Definition | Target / alert |
| --- | --- | --- |
| Open/close latency | p95 of `POST /v1/pos/shifts` and `…/close` | ≤ 100 ms |
| Close blocked | `409 shift_has_unfinished_orders`, per outlet | Track; a steady rate means orders are left in `ready` or `pending_payment` |
| Open conflicts | `409 shift_already_open`, per outlet | A rise means devices are losing track of the open shift |

### 7.2 Control signals

| Metric | Definition | Alert |
| --- | --- | --- |
| Long-running shift | An open shift older than 18 h | Alert per outlet. Someone forgot to close |
| Closed from another device | `shift.closed` where `closed_device_id ≠ device_id` | Listed on the owner dashboard; usually means a device was replaced mid-shift |
| Step-up opens/closes | Open or close where `approved_by ≠ actor_id` | Per approver, per outlet; surfaced with the void/discount report |
| Days without a shift | An active outlet with no shift on a business date it has hours for | Daily report |

---

## 8. Data Model — Migration `0006_shifts`

Differences from the overview DDL are marked `-- Δ`.

```sql
-- No opening float, no drawer: this business takes no cash.
CREATE TABLE shifts (
  id               UUID PRIMARY KEY,                       -- client-generated, retry key (FR-OPN-2)
  outlet_id        UUID NOT NULL REFERENCES outlets(id),
  device_id        UUID NOT NULL REFERENCES devices(id),   -- Δ NOT NULL: always opened on a POS
  business_date    DATE NOT NULL,
  opened_by        UUID NOT NULL REFERENCES staff(id),
  opened_at        TIMESTAMPTZ NOT NULL,
  closed_by        UUID REFERENCES staff(id),
  closed_at        TIMESTAMPTZ,
  closed_device_id UUID REFERENCES devices(id),                       -- Δ always closed on a POS
  status           TEXT NOT NULL DEFAULT 'open'
                   CHECK (status IN ('open', 'closed')),               -- Δ CHECK added
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),                 -- Δ
  -- Δ close fields are all set or all null
  CHECK ((status = 'open') = (closed_at IS NULL)),
  CHECK ((status = 'open') = (closed_by IS NULL)),
  CHECK ((status = 'open') = (closed_device_id IS NULL))
);

-- Only one shift may be open per outlet at a time (FR-SH-3).
CREATE UNIQUE INDEX one_open_shift_per_outlet
  ON shifts (outlet_id) WHERE status = 'open';

-- Δ Dashboard list by business date (FR-RD-2).
CREATE INDEX shifts_outlet_business_date_idx
  ON shifts (outlet_id, business_date DESC, opened_at DESC);

-- Atomic sequence allocation: UPDATE ... RETURNING last_value (FR-SEQ-2).
-- Rows created at shift open, one per prefix (FR-OPN-3).
CREATE TABLE order_sequences (
  shift_id   UUID NOT NULL REFERENCES shifts(id),
  prefix     TEXT NOT NULL,               -- outlets.order_prefix | 'WEB'
  last_value INT  NOT NULL DEFAULT 0 CHECK (last_value >= 0),   -- Δ CHECK added
  PRIMARY KEY (shift_id, prefix)
);
```

`shift_tender_reconciliations` is **not** created here; it belongs to Shift Settlement.

**Redis:** none.

**New error codes** (`internal/domain/errors.go`): `shift_already_open` (409), `shift_has_unfinished_orders` (409), `shift_unavailable` (503). `shift_closed` already exists and maps to 409.

**API:**

```
# POS (pos audience, RequireActiveDevice, RequireActingStaff)
GET    /v1/pos/shifts/current
POST   /v1/pos/shifts                          # open
POST   /v1/pos/shifts/{shift_id}/close

# Dashboard — per outlet, read-only
GET    /v1/outlets/{outlet_id}/shifts
GET    /v1/outlets/{outlet_id}/shifts/{shift_id}
```

Δ The overview's indicative `POST /v1/shifts/open` and `POST /v1/shifts/{id}/close` move under `/v1/pos`, like the catalog bundle and sold-out routes, so the outlet comes from the device token.

**Internal (package `internal/shift`):**

```go
// The caller's transaction (or pool); pgx.Tx and *pgxpool.Pool both satisfy it.
type Querier interface {
    Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
    QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// OpenShift is what a placement needs: ID, OutletID, BusinessDate ("YYYY-MM-DD").
func Current(ctx context.Context, q Querier, outletID string) (*OpenShift, error)            // FOR SHARE; ErrNoOpenShift
func NextNumber(ctx context.Context, q Querier, shiftID, prefix string) (int, string, error) // ErrShiftClosed, ErrUnknownSequence
func FormatNumber(prefix string, seq int) string                                             // "POS-001"

type OrderGuard interface {
    // Up to limit unfinished orders of the shift, and the total count.
    UnfinishedOrders(ctx context.Context, q Querier, shiftID string, limit int) ([]OrderRef, int, error)
}
```

IDs are canonical UUID strings, as in the rest of the codebase's services. `shift.Querier` has the same shape as `catalog.Querier`, so one transaction serves both `Quote` and these calls. `shift.NoOrders{}` is the guard stub wired in `cmd/api/main.go`; Orders replaces it there.

The step-up check lives in `internal/authz`:

```go
// Returns the approver's staff ID: the acting staff member if they hold perm
// at the outlet, otherwise approvedBy if that person does. Else ErrForbidden.
func Approver(ctx context.Context, src AccessSource, acting *domain.StaffAccess,
    outletID, perm, approvedBy string) (string, error)
```

---

## 9. Risks and Accepted Trade-offs

| Risk | Status |
| --- | --- |
| **No connection, no shift, no sales.** If the outlet has no connectivity at opening time, the POS cannot sell at all. | Accepted by decision. Staff must connect the device (outlet Wi-Fi, mobile hotspot). Once a shift is open, selling continues offline. |
| **Unfinished orders block close.** A forgotten `ready` order or a slow `pending_payment` web order keeps the shift open. | Accepted by decision until settlement exists. The `409` lists the orders so staff can resolve them; the long-running-shift alert (§7.2) catches forgotten shifts. Revisit when Shift Settlement defines close. |
| **Orders the server can't see.** Open tickets and orders queued on the POS are invisible to the server guard. | Mitigated by the POS pre-close checks (FR-CLS-2): ongoing orders first, then connection, then a full sync, then close. With no dashboard close, only a buggy or tampered POS can bypass this. **Orders must still decide** how a synced order referencing a closed shift is handled; recommended: reject it into a flagged state for a manager to review. |
| **Lost device, stuck shift.** With no dashboard close, a shift stays open until a device at the outlet closes it. | Accepted by decision. A manager enrolls a replacement device, which closes the shift (FR-SH-8). Orders that existed only on the lost device are lost; the long-running-shift alert (§7.2) surfaces the problem. |
| **Business date fixed at open.** Sales after the cutoff in a late-running shift count toward the opening day. | Accepted, and **recommended for Orders**: orders take their shift's `business_date`, so one shift's sales never split across two days and the settlement matches the daily report. Orders must confirm this. |
| **The approver's PIN is checked only on the device.** The server checks the approver's permission, not that they were present. | Accepted. Same trust model as `X-Acting-Staff-Id` (open item M3 from the 2026-11-02 scan). Revisit with acting-staff proof. |
| **One open shift per outlet.** A second POS device would share the shift. | Accepted; matches "one POS device per outlet". Sequences are per prefix, so a second device with its own prefix numbers independently inside the same shift. |
| **Web orders while closed.** `Current` returns no shift between close and the next open. | Out of scope here; Orders implements the overview's "queue to the next shift". |
