# Orders Core — Feature Spec

## 1. Introduction

This feature lets the POS take an order from entry to payment while it is online. It adds:

- placing a counter order into the open shift, priced by `catalog.Quote`, with a per-shift order number
- the **open bill**: a placed order that is not paid yet, to which lines can be added and from which lines can be removed
- cancelling an unpaid order
- recording the payment (static QRIS or card on EDC), which completes the order
- order reads for the POS and the dashboard
- the real shift close guard, replacing the `shift.NoOrders{}` stub
- outbox events for every order change, written but not yet consumed

The counter flow this spec implements:

1. The cashier enters the order and taps **Process order**. The order is created on the server as `placed`, gets its number, and the kitchen can be notified (how is decided later; the event is already written).
2. Until payment, the cashier can add lines, remove lines, or cancel the whole order.
3. The cashier confirms the amount and records the payment. The order becomes `completed`.
4. After payment a cashier can change nothing. Voiding a paid order needs someone above cashier (`order.void.paid`: supervisor, manager, owner), on the POS with step-up or from the dashboard, and is specified with Refunds + Paid Void.

**Depends on:**

- `catalog.Quote` with outlet overrides and sold-out (Catalog Core, Outlet Catalog)
- `shift.Current`, `shift.NextNumber`, `shift.OrderGuard` (Shifts Core)
- POS devices, `RequireActiveDevice`, `RequireActingStaff` (auth slice 3)
- outlet-scoped authorization (`internal/authz`) and `audit_logs` (migration `0002_authz`)

**Related (later specs, in the order recommended):**

- **POS Controls** — discounts, price override, step-up via `authz.Approver`
- **Offline Sync** — `/v1/sync/batch`, `Quote` in `Record` mode, `authz.offline_violation`
- **Web Orders + Midtrans** — guest and customer checkout, `pending_payment`, webhook, expiry
- **Refunds + Paid Void** — void of a paid order by a holder of `order.void.paid`, on the POS with step-up and from the dashboard; refund workflow

### 1.1 Goals

- A cashier can place, change, cancel and take payment for an order with one request per step.
- Every order belongs to exactly one shift and has a number unique within that shift.
- A retried request never creates a second order, line, payment or number.
- What was charged can always be reproduced from the order's own rows, whatever happens to the catalog afterwards.
- Every removed line and cancelled order leaves a trail: who, when, why, and how much.
- A shift cannot close while it has an unpaid order.

### 1.2 In scope

- Tables `orders`, `order_lines`, `order_line_modifiers`, `order_status_events`, `payments`, `outbox_events`
- POS: place an order, add lines, void a line, cancel an order, record a payment, list and read orders
- Dashboard: list and read an outlet's orders (read-only)
- Totals calculation
- `OrderGuard` implementation and shifts AC-11
- Auditing, error contract and metrics for the above

### 1.3 Out of scope

- **Selling while offline.** Until Offline Sync exists, the POS needs a connection to place an order or record a payment (see §9).
- **Discounts and price override** → POS Controls. The discount columns exist and stay 0.
- **Voiding a paid order, refunds** → Refunds + Paid Void. This spec only records the rule (§1.4).
- **Web orders**, Midtrans, `pending_payment`, `expired`, tracking token, guest fields, guest linking (auth FR-CUS-1), customer scope (auth FR-AZ-9/10) → Web Orders + Midtrans
- **Kitchen statuses.** `preparing` and `ready` are allowed by the schema but nothing sets them. How the kitchen is notified is decided later.
- **Tax, service charge and packaging fee.** No charges at launch. The columns exist and stay 0; rates and their configuration get their own change when the business introduces a charge.
- Changing a line in place (quantity, modifiers, fulfillment type). Remove the line and add a new one.
- Editing the order header (`table_number`, `note`) after placement
- Moving lines between orders, splitting or merging bills
- Split payments. One payment per order (overview scope decision)
- Outbox worker and consumers (`cmd/worker`, Asynq)
- `Idempotency-Key` header and its Redis store. Needed first by Web Orders, which has no client-generated ID to rely on
- Receipts

### 1.4 Decisions carried from the project overview, earlier specs and the user

| Decision | Source |
| --- | --- |
| One order model for all channels; fulfillment type lives on the line | Overview §E |
| Order ID is a client-generated UUID; the number is a display label | Overview §E |
| Number allocated at placement, by `shift.NextNumber`, inside the order's transaction | Overview §E, shifts FR-SEQ |
| Line snapshots are immutable; money is `BIGINT` rupiah | Overview §J |
| **Orders take their shift's `business_date`** | Recommended in shifts-spec §9, confirmed here |
| **The first Orders spec covers POS orders while online, with in-store payment** | Decided for this feature |
| **An open bill lives on the server** from the moment it is processed, numbered, and lines can be added until payment | Decided for this feature |
| **Payment completes a POS order.** There is no separate handover step | Decided for this feature |
| **Before payment, any cashier can remove a line or cancel the order**, with a reason | Decided for this feature |
| **After payment, only a role above cashier can void** (`order.void.paid`: supervisor, manager, owner, as seeded). **Both on the POS with step-up** (auth spec FR-STEP-1, a cashier acts with an approver who holds the permission) **and from the dashboard.** Built with Refunds + Paid Void | Decided for this feature |
| **Payment reference (RRN) is optional** | Decided for this feature |
| **Retries are made safe by client-generated IDs**, not an `Idempotency-Key` header | Decided for this feature. Deviates from overview §J for POS routes |
| **Outbox events are written now, with no worker** | Decided for this feature |
| **No tax, service charge or packaging fee at launch** | Decided for this feature |

### 1.5 Vocabulary

| Term | Meaning |
| --- | --- |
| Ticket | An order being entered on the POS, before *Process order*. Exists only on the device |
| Open bill | An order with `status = placed`: processed, numbered, not paid |
| Active line | A line with `status = active`. Voided lines stay on the order but count toward nothing |
| Void a line | Remove one line from an open bill |
| Cancel an order | End an open bill without payment. `status = cancelled` |
| Unfinished order | An order that blocks shift close (FR-GRD-1) |
| Acting staff | The staff member operating the POS, from `X-Acting-Staff-Id` |

---

## 2. Actors and Access

| Actor | Client | What they do | Audience | Permission |
| --- | --- | --- | --- | --- |
| Cashier and above | POS | Place an order, add lines | `pos` + acting staff | `order.create` (outlet) |
| Cashier and above | POS | Void a line, cancel an open bill | `pos` + acting staff | `order.void.unpaid` (outlet) |
| Cashier and above | POS | Record a payment | `pos` + acting staff | `payment.record` (outlet) |
| Any outlet staff | POS | List and read the outlet's orders | `pos` + acting staff | any role at the outlet |
| Any staff with a role at the outlet | Dashboard | List and read orders | `staff` | any role at the outlet |

All three permissions are already seeded for every role (migration `0001_auth`), so no action in this spec needs step-up, and no route accepts `approved_by`.

`order.void.paid` (supervisor, manager, owner) is not used by any route in this spec. Role permissions are unchanged.

---

## 3. Functional Requirements

Requirement IDs are stable and referenced by the acceptance criteria in §6.

### 3.1 Lifecycle

- **FR-ORD-1** In this slice a POS order moves:

  ```
  [*] → placed → completed     (payment recorded)
        placed → cancelled     (cancelled before payment)
  ```

  `completed` and `cancelled` are final. The other statuses of the overview (`draft`, `pending_payment`, `preparing`, `ready`, `expired`, `refunded`) are allowed by the schema for later specs; nothing here sets them.
- **FR-ORD-2** There are **no server-side drafts**. A ticket exists only on the POS until *Process order*.
- **FR-ORD-3** An order is **paid** when `paid_at` is set. In this slice that is exactly when it is `completed`. Web Orders will have paid orders that are not yet completed, which is why paid is its own column.
- **FR-ORD-4** Every status change writes one `order_status_events` row in the same transaction, with the acting staff member, and the reason for a cancellation.
- **FR-ORD-5** All timestamps are **server time**. Requests carry no client time, because the device is online.
- **FR-ORD-6** Every route that changes an order locks its row `FOR UPDATE` first, so changes to one order are applied one at a time.
- **FR-ORD-7** A change to an order that is not `placed` returns `409 order_not_open`, with the current status in `details`, unless it is a retry covered by FR-ADD-3, FR-VOID-4, FR-CAN-3 or FR-PAY-5.

### 3.2 Placing an order (POS)

- **FR-PLC-1** `POST /v1/pos/orders` with `X-Acting-Staff-Id` and body:

  ```json
  {
    "id": "<client-generated UUID>",
    "table_number": "12",
    "note": "…",
    "lines": [
      {
        "id": "<client-generated UUID>",
        "product_id": "…",
        "variant_id": "…",
        "modifier_ids": ["…"],
        "quantity": 2,
        "fulfillment_type": "dine_in",
        "note": "no ice"
      }
    ]
  }
  ```

  The outlet and device come from the device token. `table_number`, `note`, `variant_id`, `modifier_ids` and the line `note` are optional.
- **FR-PLC-2** **Validation** (`400 invalid_input`, per field):
  - 1 to 100 lines; line IDs unique within the request
  - `quantity` 1 to 999
  - `fulfillment_type` is `dine_in` or `takeaway`
  - `table_number` at most 20 characters; notes at most 280 characters
- **FR-PLC-3** **Lines are kept as sent.** The server never merges lines. Two of the same product with different fulfillment types are two lines (overview §E).
- **FR-PLC-4** In one transaction the server:
  1. takes the outlet's open shift with `shift.Current` (`FOR SHARE`); none → `409 no_open_shift`
  2. prices the lines with `catalog.Quote`, `Channel = pos`, `Mode = Strict`, `At` = now
  3. allocates the number with `shift.NextNumber` and the outlet's `order_prefix`
  4. inserts the order (`status = placed`, `channel = pos`, `placed_at` = now, `business_date` = the shift's), its lines and their modifiers
  5. writes the status event (FR-ORD-4) and the `order.placed` outbox event (FR-EVT-1)
- **FR-PLC-5** **Unavailable lines.** If `Quote` reports a problem on any line, nothing is written and the response is `409 lines_unavailable`. `details.lines` lists each failing line: its `id`, the problem `kind` (the `catalog.ProblemKind` value), and the offending variant, modifier or group ID when there is one. The POS shows which lines to fix.
- **FR-PLC-6** **Idempotent by `id`.** The client generates the order ID once and reuses it on every retry:

  | Existing state | Response |
  | --- | --- |
  | No order with this `id` | `201`, order created, `applied: true` |
  | This `id` exists at this outlet | `200`, `applied: false`, the order as stored. The request body is not compared. No number is allocated |
  | This `id` exists at another outlet | `409 conflict` |

- **FR-PLC-7** The response is the order (§3.7 shape) with `applied`.

### 3.3 Totals

- **FR-TOT-1** Each line stores the `Quote` snapshot: `product_name`, `category_id`, `category_name`, `variant_name`, `unit_price`, `unit_cost`, and one `order_line_modifiers` row per modifier with its name and price delta. `unit_price` already includes the variant and modifier deltas.
- **FR-TOT-2** `line_total = unit_price × quantity − discount_amount`. `discount_amount` is 0 in this slice.
- **FR-TOT-3** Order totals are computed from **active lines only** and stored on the order:
  - `subtotal` = Σ `line_total`
  - `grand_total` = `subtotal − discount_total + service_charge + packaging_fee + tax_total`

  `discount_total`, `service_charge`, `packaging_fee` and `tax_total` are 0 and `tax_breakdown` is `{}` in this slice, so `grand_total = subtotal`.
- **FR-TOT-4** `fulfillment_summary` is derived from active lines: `dine_in`, `takeaway`, or `mixed`.
- **FR-TOT-5** Totals and `fulfillment_summary` are recomputed in the same transaction whenever lines are added or voided. Snapshots of existing lines are never re-priced.
- **FR-TOT-6** Prices are never taken from the client. The request carries no price field.

### 3.4 Changing an open bill (POS)

**Adding lines**

- **FR-ADD-1** `POST /v1/pos/orders/{order_id}/lines` with body `{ "lines": [ … ] }`, lines as in FR-PLC-1. Needs `order.create`.
- **FR-ADD-2** The new lines are validated as in FR-PLC-2 and priced by `Quote` at the current time and catalog. The order may hold at most 100 lines in total, voided ones included; over that → `400 invalid_input`. Problems → `409 lines_unavailable`, nothing written.
- **FR-ADD-3** **Idempotent by line ID.** If every line ID in the request already exists on this order, the response is `200`, `applied: false`. If only some exist, or a line ID belongs to another order, the response is `409 conflict` and nothing is written.
- **FR-ADD-4** Each line records who added it (`created_by`). Writes the `order.updated` outbox event.

**Voiding a line**

- **FR-VOID-1** `POST /v1/pos/orders/{order_id}/lines/{line_id}/void` with body `{ "reason": "…" }`. Needs `order.void.unpaid`. `reason` is required, 3 to 280 characters.
- **FR-VOID-2** The line becomes `voided` with `voided_by`, `voided_at` and `void_reason`. It stays on the order and is returned in reads. Totals are recomputed (FR-TOT-5).
- **FR-VOID-3** The last active line cannot be voided: `409 last_active_line`. The cashier cancels the order instead, so an order never has a zero-line bill to pay.
- **FR-VOID-4** Voiding an already-voided line returns `200`, `applied: false`.
- **FR-VOID-5** Writes an audit row (FR-OAUD-1) and the `order.updated` outbox event.

**Cancelling**

- **FR-CAN-1** `POST /v1/pos/orders/{order_id}/cancel` with body `{ "reason": "…" }`. Needs `order.void.unpaid`. `reason` is required, 3 to 280 characters.
- **FR-CAN-2** The order becomes `cancelled`. Its lines and stored totals are left as they were, so the record shows what was cancelled. The number is not reused.
- **FR-CAN-3** Cancelling an already-cancelled order returns `200`, `applied: false`. Cancelling a completed order returns `409 order_not_open`.
- **FR-CAN-4** Writes an audit row (FR-OAUD-1) and the `order.cancelled` outbox event.

All three routes return the order (§3.7 shape) with `applied`.

### 3.5 Payment (POS)

- **FR-PAY-1** `POST /v1/pos/orders/{order_id}/payments` with body:

  ```json
  { "id": "<client-generated UUID>", "method": "qris_static", "amount": 54000, "external_ref": "…" }
  ```

  Needs `payment.record`. `method` is `qris_static` or `card_edc`. `external_ref` (the RRN from the merchant app or EDC slip) is optional, at most 64 characters.
- **FR-PAY-2** **The POS records, it does not verify.** The payment is stored as `settled` at once, with `recorded_by` = the acting staff member and `settled_at` = now. Nothing checks it against a bank or provider (overview §F).
- **FR-PAY-3** **`amount` must equal the order's `grand_total`.** Otherwise `409 amount_mismatch`, with the current `grand_total` in `details`, and nothing is written. This catches a payment taken against a total that changed (a line added or voided in between).
- **FR-PAY-4** In the same transaction the order gets `paid_at` = now and `status = completed`, with a status event and the `order.paid` outbox event.
- **FR-PAY-5** **Idempotent by payment `id`:**

  | Existing state | Response |
  | --- | --- |
  | Order is `placed`, no payment | `201`, `applied: true` |
  | A payment with this `id` exists on this order | `200`, `applied: false` |
  | Order is already paid by a payment with a different `id` | `409 order_already_paid` |
  | Order is `cancelled` | `409 order_not_open` |
  | This payment `id` exists on another order | `409 conflict` |

- **FR-PAY-6** One settled payment per order, enforced by a partial unique index. The table stays one-to-many for later (overview scope decision).
- **FR-PAY-7** The response is the order (§3.7 shape) with `applied`.

### 3.6 Shift close guard

- **FR-GRD-1** **Unfinished** means any status other than `completed`, `cancelled`, `expired` or `refunded`. In this slice that is `placed`: an open bill. The list is defined once, in `internal/order`.
- **FR-GRD-2** `internal/order` implements `shift.OrderGuard`: up to `limit` unfinished orders of the shift, oldest first, and the total count. `cmd/api/main.go` wires it in place of `shift.NoOrders{}`.
- **FR-GRD-3** Placement holds the shift row `FOR SHARE` (FR-PLC-4), so a close either sees the new order or the placement sees no open shift (shifts FR-CLS-4). Adding lines, voiding, cancelling and paying do not lock the shift: they only apply to a `placed` order, and a shift with a `placed` order cannot have closed.

### 3.7 Reads

- **FR-RD-1** `GET /v1/pos/orders?status=&shift_id=` lists the device's outlet's orders, newest first, cursor-paginated (default 50, max 200).
  - `shift_id` defaults to the outlet's open shift. With no open shift and no `shift_id`, the list is empty.
  - `status` is optional: `placed`, `completed` or `cancelled`. `status=placed` is the open-bills list, which the POS loads at start-up, on reconnect, and on a replacement device.
- **FR-RD-2** `GET /v1/pos/orders/{order_id}` returns one order of the device's outlet.
- **FR-RD-3** `GET /v1/outlets/{outlet_id}/orders?from=&to=&status=&shift_id=&order_number=` lists orders by `business_date` range (inclusive, at most 92 days, default today), newest first, cursor-paginated. `order_number` is an exact match within the range, since numbers repeat across shifts (overview risks).
- **FR-RD-4** `GET /v1/outlets/{outlet_id}/orders/{order_id}` returns one order.
- **FR-RD-5** **Shape.** Lists return the header without `lines`, `payments` and `events`; single reads return everything.

  ```json
  {
    "id": "…", "outlet_id": "…", "shift_id": "…", "device_id": "…",
    "order_number": "POS-012", "channel": "pos",
    "status": "placed", "fulfillment_summary": "mixed",
    "business_date": "2026-11-12",
    "table_number": "12", "note": null,
    "subtotal": 54000, "discount_total": 0, "service_charge": 0,
    "packaging_fee": 0, "tax_total": 0, "grand_total": 54000,
    "placed_at": "2026-11-12T03:10:00Z", "paid_at": null,
    "created_by": { "id": "…", "name": "Rina" },
    "lines": [
      {
        "id": "…", "product_id": "…", "variant_id": "…",
        "product_name": "Es Kopi Susu", "variant_name": "Large",
        "category_id": "…", "category_name": "Coffee",
        "unit_price": 27000, "quantity": 2, "discount_amount": 0, "line_total": 54000,
        "fulfillment_type": "dine_in", "note": "no ice",
        "modifiers": [ { "id": "…", "name": "Less sugar", "price_delta": 0 } ],
        "status": "active",
        "created_by": { "id": "…", "name": "Rina" }, "created_at": "…",
        "voided_by": null, "voided_at": null, "void_reason": null
      }
    ],
    "payments": [
      { "id": "…", "method": "qris_static", "amount": 54000, "status": "settled",
        "external_ref": null, "recorded_by": { "id": "…", "name": "Rina" }, "settled_at": "…" }
    ],
    "events": [ { "from": null, "to": "placed", "reason": null, "staff": { "id": "…", "name": "Rina" }, "at": "…" } ]
  }
  ```

- **FR-RD-6** `unit_cost` is stored but never returned in this slice. Cost and margin belong to reporting, behind `cost.view`.

### 3.8 Authorization summary

| Route | Audience | Requirement | No role at outlet |
| --- | --- | --- | --- |
| `POST /v1/pos/orders` | `pos` + acting staff | `order.create` | `403` |
| `POST /v1/pos/orders/{order_id}/lines` | `pos` + acting staff | `order.create` | `403` |
| `POST /v1/pos/orders/{order_id}/lines/{line_id}/void` | `pos` + acting staff | `order.void.unpaid` | `403` |
| `POST /v1/pos/orders/{order_id}/cancel` | `pos` + acting staff | `order.void.unpaid` | `403` |
| `POST /v1/pos/orders/{order_id}/payments` | `pos` + acting staff | `payment.record` | `403` |
| `GET /v1/pos/orders`, `GET /v1/pos/orders/{order_id}` | `pos` + acting staff | any role at the device's outlet | `403` |
| `GET /v1/outlets/{outlet_id}/orders`, `GET …/orders/{order_id}` | `staff` | any role at the outlet | `404` |

Permissions are checked server side at request time. Every POS route also runs `RequireActiveDevice`. An order of another outlet behaves as unknown (`404`).

### 3.9 Audit

- **FR-OAUD-1** Line voids and cancellations each write one `audit_logs` row in the **same transaction**, with `outlet_id`, `actor_type = staff`, `actor_id` = the acting staff member, `approved_by` = the actor, and `reason`:

  | Action | `entity_type` / `entity_id` | `value_delta` | `after` |
  | --- | --- | --- | --- |
  | `order.line_voided` | `order` / the order ID | `−line_total` | `line_id`, `product_name`, `quantity`, `order_number` |
  | `order.cancelled` | `order` / the order ID | `−grand_total` | `order_number`, active line count |

- **FR-OAUD-2** Placing an order, adding lines and recording a payment write no audit row. The order, line and payment rows already say who did it and when.
- **FR-OAUD-3** No-op retries and rejected requests write no audit row, status event or outbox event.

### 3.10 Outbox events

- **FR-EVT-1** Each change writes one `outbox_events` row in the same transaction, `aggregate_type = order`, `aggregate_id` = the order ID:

  | Event | When |
  | --- | --- |
  | `order.placed` | Order placed |
  | `order.updated` | Lines added, or a line voided |
  | `order.cancelled` | Order cancelled |
  | `order.paid` | Payment recorded |

- **FR-EVT-2** The payload carries `v: 1`, `order_id`, `outlet_id`, `shift_id`, `business_date`, `channel`, `order_number`, `status` and `grand_total`; `order.updated` adds the affected `line_ids`. Consumers read the rest from the order.
- **FR-EVT-3** Nothing publishes these rows in this slice. They stay with `published_at` null until a worker exists, so inventory, reports and the kitchen notification can start from the first order ever placed.

---

## 4. Non-Functional Requirements

### 4.1 Security

- **NFR-SEC-1** POS routes are scoped to the device's outlet, taken from the token, never from the body.
- **NFR-SEC-2** Prices, totals, order numbers, statuses and timestamps are never accepted from the client.
- **NFR-SEC-3** JSON request bodies are capped at 64 KB on placement and add-lines, 4 KB elsewhere.
- **NFR-SEC-4** `reason`, notes and `external_ref` are stored as plain text and never interpreted.

### 4.2 Performance

- **NFR-PERF-1** Placement of a 20-line order: p95 ≤ 150 ms. Lines and modifiers are inserted in batches, not one statement per row.
- **NFR-PERF-2** Add lines, void, cancel, payment: p95 ≤ 100 ms.
- **NFR-PERF-3** `GET /v1/pos/orders?status=placed`: p95 ≤ 50 ms with 200 orders in the shift. Reads of one order use a fixed number of queries, whatever the line count.

### 4.3 Availability and resilience

- **NFR-AV-1** This feature uses no Redis of its own.

  | Failure | Behaviour |
  | --- | --- |
  | Postgres down | `503 order_unavailable` |
  | Commit outcome uncertain (`postgres.ErrCommitUncertain`) | `503 order_unavailable`. The client retries with the same IDs; FR-PLC-6, FR-ADD-3, FR-VOID-4, FR-CAN-3 and FR-PAY-5 make the retry safe |
  | Device offline | The POS cannot process an order or record a payment until Offline Sync exists (§9) |

- **NFR-AV-2** A failed placement burns no order number: the allocation rolls back with the transaction (shifts FR-SEQ-5).

### 4.4 Operability and testability

- **NFR-OPS-1** The clock is injectable.
- **NFR-OPS-2** Totals calculation is a pure function with a table-driven unit test.
- **NFR-OPS-3** Integration tests run against `TEST_DATABASE_URL`, as in the catalog and shift features.
- **NFR-OPS-4** OpenAPI and the metrics in §7 follow once those exist (deferred project-wide).

---

## 5. Edge Cases and Error Handling

### 5.1 Error contract

Uses the existing envelope. Codes used here:

| HTTP | `code` | When | Notes |
| --- | --- | --- | --- |
| 400 | `invalid_input` | Bad UUID, quantity, fulfillment type, length, line count, date range | Per field |
| 401 | `device_revoked` | POS route from a revoked device | Existing device check |
| 403 | `forbidden` | Acting staff lacks the permission or has no role at the outlet | |
| 404 | `not_found` | Unknown order or line, order at another outlet, no role at the outlet (dashboard) | |
| 409 | `no_open_shift` | **New.** Placement with no open shift at the outlet | |
| 409 | `lines_unavailable` | **New.** `Quote` rejected one or more lines | `details.lines`: `id`, `kind`, offending ID |
| 409 | `order_not_open` | **New.** Change to a completed or cancelled order | `details.status` |
| 409 | `last_active_line` | **New.** Voiding the only active line | |
| 409 | `amount_mismatch` | **New.** Payment amount ≠ `grand_total` | `details.grand_total` |
| 409 | `order_already_paid` | **New.** A second payment with a different `id` | |
| 409 | `conflict` | An order, line or payment `id` that exists elsewhere; add-lines with a mix of new and existing IDs | |
| 413 | `payload_too_large` | Body over the cap | |
| 503 | `order_unavailable` | **New.** Postgres down or commit uncertain | |
| 503 | `catalog_unavailable` | Existing. `Quote` could not read the catalog | |

### 5.2 Edge cases

**Placing**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-1 | First order of a shift | `POS-001`, `status = placed`, `business_date` = the shift's |
| E-2 | Shift opened yesterday evening, order placed after today's cutoff | `business_date` is still the shift's opening day |
| E-3 | Network drops after the commit; POS retries with the same `id` | `200`, `applied: false`, same number, one order |
| E-4 | Retry with the same `id` but different lines | `200`, `applied: false`, the order as first stored. The POS must not reuse an order ID for a different ticket |
| E-5 | A product is marked sold out between entry and *Process order* | `409 lines_unavailable`, `kind = product_sold_out`, nothing written, no number used |
| E-6 | Breakfast item ordered at 11:01 with a window ending 11:00 | `409 lines_unavailable`, `kind = outside_window` |
| E-7 | No shift is open | `409 no_open_shift` |
| E-8 | Placement and shift close race | Either the order commits and blocks the close, or the close commits and placement gets `409 no_open_shift` |
| E-9 | Same product twice, one dine-in and one takeaway | Two lines; `fulfillment_summary = mixed` |
| E-10 | The catalog price changes after placement | The line keeps its snapshot. Lines added later get the new price |
| E-11 | Every line has a zero price | Order placed with `grand_total = 0`. It is completed by a payment with `amount = 0` (see §9) |
| E-12 | Two devices place with the same order `id` at different outlets | The second gets `409 conflict` |

**Changing an open bill**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-13 | Customer adds a drink to an open bill | New line priced now; totals recomputed; number unchanged |
| E-14 | Add-lines retried after a lost response | `200`, `applied: false`, no duplicate lines |
| E-15 | Customer drops one of three items | Line `voided` with reason; totals drop by its `line_total`; audit row with that amount |
| E-16 | Customer drops the only item | `409 last_active_line`; the cashier cancels instead |
| E-17 | Customer wants 3 instead of 2 of a line | Void the line, add a new one with quantity 3 |
| E-18 | Customer leaves before paying | Cashier cancels with a reason; `order.cancelled` audit row with `−grand_total` |
| E-19 | Void or cancel without a reason | `400 invalid_input` |
| E-20 | Line added while another request pays the order | The order row lock serialises them. If the add wins, the payment gets `409 amount_mismatch`. If the payment wins, the add gets `409 order_not_open` |
| E-21 | Line ID from another order sent to the void route | `404 not_found` |

**Payment**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-22 | Payment recorded with the exact total | `201`; payment `settled`; order `completed`, `paid_at` set |
| E-23 | POS shows an old total after a line was added on reconnect | `409 amount_mismatch` with the current `grand_total`; the POS refreshes and the cashier confirms again |
| E-24 | Payment retried after a lost response | `200`, `applied: false`, one payment |
| E-25 | Cashier taps pay twice and the POS generates two payment IDs | The second gets `409 order_already_paid` |
| E-26 | Payment on a cancelled order | `409 order_not_open` |
| E-27 | No reference entered | Accepted; `external_ref` is null |
| E-28 | Cashier tries to void a line after payment | `409 order_not_open`. No route in this slice voids a paid order |

**Shift close**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-29 | Close with an open bill | `409 shift_has_unfinished_orders` listing it. Staff take payment or cancel, then close |
| E-30 | Close with only completed and cancelled orders | Close succeeds |
| E-31 | Replacement device after the first one broke mid-shift | It loads the open bills with `GET /v1/pos/orders?status=placed` and can pay, change or cancel them |

---

## 6. Acceptance Criteria

Each criterion is an automated integration test unless marked *(manual)*.

### Placing

- **AC-1** *(FR-PLC-4, FR-TOT-1)* A cashier places a two-line order with a variant and modifiers: `201`, `status = placed`, `POS-001`, the shift's `business_date`, lines holding the `Quote` snapshot (names, category, `unit_price`, `unit_cost`), modifier rows, one status event and one `order.placed` outbox row.
- **AC-2** *(FR-PLC-6)* Retrying with the same `id` returns `200`, `applied: false`, the same number, and leaves one order, one status event, one outbox row, and the sequence unchanged.
- **AC-3** *(FR-PLC-5, NFR-AV-2)* An order with one sold-out line returns `409 lines_unavailable` naming that line and kind. Nothing is written and the next order still gets the next number without a gap.
- **AC-4** *(FR-PLC-4)* With no open shift, placement returns `409 no_open_shift`.
- **AC-5** *(FR-PLC-2)* A table-driven test rejects: no lines, 101 lines, quantity 0 and 1000, an unknown fulfillment type, duplicate line IDs, an over-long note.
- **AC-6** *(FR-TOT-2..4)* A unit test table covers totals and `fulfillment_summary`: all dine-in, all takeaway, mixed, with voided lines excluded.
- **AC-7** *(FR-TOT-5, E-10)* After a catalog price change, existing lines keep their `unit_price` and a line added afterwards has the new price.
- **AC-8** *(FR-PLC-3)* Two lines of the same product with different fulfillment types are stored as two lines.
- **AC-9** *(FR-PLC-6)* Twenty concurrent placements with distinct IDs produce twenty orders with distinct, gapless numbers. Two concurrent placements with the same ID produce one order.

### Changing an open bill

- **AC-10** *(FR-ADD-1..4)* Adding a line to an open bill updates the totals, records `created_by`, and writes `order.updated`. Retrying returns `200`, `applied: false` with no duplicate. A request mixing an existing and a new line ID returns `409 conflict`.
- **AC-11** *(FR-VOID-1..5, FR-OAUD-1)* Voiding one of two lines sets the line's void fields, lowers `subtotal` and `grand_total` by its `line_total`, and writes one audit row with `value_delta = −line_total` and the reason. A retry returns `200`, `applied: false` and writes no second audit row.
- **AC-12** *(FR-VOID-3)* Voiding the last active line returns `409 last_active_line`.
- **AC-13** *(FR-CAN-1..4, FR-OAUD-1)* Cancelling an open bill sets `cancelled`, keeps lines and totals, and writes a status event with the reason, an audit row with `value_delta = −grand_total`, and `order.cancelled`. A retry returns `200`, `applied: false`.
- **AC-14** *(FR-VOID-1, FR-CAN-1)* A void or cancel without a reason returns `400` and writes nothing.

### Payment

- **AC-15** *(FR-PAY-1..4)* Recording a payment equal to `grand_total` returns `201`; the payment is `settled` with `recorded_by`; the order is `completed` with `paid_at`; a status event and `order.paid` are written.
- **AC-16** *(FR-PAY-3)* A payment with any other amount returns `409 amount_mismatch` with the current `grand_total`, and nothing is written.
- **AC-17** *(FR-PAY-5, FR-PAY-6)* Retrying with the same payment `id` returns `200`, `applied: false`. A different `id` on the paid order returns `409 order_already_paid`. Two concurrent payments with different IDs leave exactly one payment.
- **AC-18** *(FR-ORD-7)* On a completed order, add lines, void and cancel each return `409 order_not_open`. On a cancelled order, add lines, void and payment each return `409 order_not_open`.
- **AC-19** *(FR-ORD-6, E-20)* An add-lines request and a payment racing on one order end in one of the two outcomes of E-20, never a completed order whose payment differs from its `grand_total`.

### Shift close

- **AC-20** *(FR-GRD-1, FR-GRD-2; shifts AC-11)* With one open bill, close returns `409 shift_has_unfinished_orders` listing it with `id`, `order_number`, `status`, `channel`. After the bill is paid, and separately after it is cancelled, close succeeds. A unit test checks that every status outside the finished list counts as unfinished.
- **AC-21** *(FR-GRD-3)* With a placement transaction open, a concurrent close waits, then returns `409` listing the new order.

### Reads and access

- **AC-22** *(FR-RD-1)* `GET /v1/pos/orders?status=placed` returns only the open shift's open bills, newest first, and an empty list when no shift is open.
- **AC-23** *(FR-RD-3)* The dashboard list filters by business-date range, status, shift and exact order number, and rejects a range over 92 days.
- **AC-24** *(FR-RD-5, FR-RD-6)* A single read returns lines (voided ones included), modifiers, payments and events, and no `unit_cost`.
- **AC-25** *(§3.8)* A route walk on the real router checks each route's audience and permission. Acting staff without the permission gets `403`. A POS request for another outlet's order gets `404`. Staff with no role at the outlet get `404` on the dashboard routes. A revoked device gets `401 device_revoked`.
- **AC-26** *(NFR-AV-1)* With the commit reported uncertain, placement returns `503 order_unavailable`, and a retry with the same `id` returns the order whether or not the first attempt committed.

---

## 7. Metrics

Prometheus is not wired in yet. These are the metrics to emit once it is. Until then, the control signals in §7.2 can be read from `orders`, `order_lines`, `payments` and `audit_logs`.

### 7.1 Health and experience

| Metric | Definition | Target / alert |
| --- | --- | --- |
| Placement latency | p95 of `POST /v1/pos/orders` | ≤ 150 ms |
| Rejected placements | `409 lines_unavailable` by problem kind | A rise in sold-out or window kinds means the POS catalog is stale |
| Amount mismatches | `409 amount_mismatch`, per outlet | A steady rate means the POS shows stale totals |
| Unpublished outbox rows | Count with `published_at` null | Grows without bound until the worker exists; watch table size |

### 7.2 Control signals

| Metric | Definition | Alert |
| --- | --- | --- |
| Voided lines | Count and value of `order.line_voided`, per staff member and outlet | Surfaced in the void/discount report; primary fraud signal (overview §F) |
| Cancelled orders | Count and value of `order.cancelled`, per staff member and outlet | Same report |
| Old open bills | `placed` orders older than 4 h | Per outlet. A bill nobody paid or cancelled |
| Payments without a reference | Share of payments with null `external_ref`, per tender and staff member | Month-end matching gets harder as this rises |

---

## 8. Data Model — Migration `0007_orders`

Differences from the overview DDL are marked `-- Δ`.

```sql
CREATE TABLE orders (
  id                  UUID PRIMARY KEY,                    -- client-generated, retry key (FR-PLC-6)
  outlet_id           UUID NOT NULL REFERENCES outlets(id),
  shift_id            UUID NOT NULL REFERENCES shifts(id), -- Δ NOT NULL; Web Orders decides whether queued web orders need null
  device_id           UUID REFERENCES devices(id),         -- null for web orders

  -- Display label only. NEVER a foreign key, NEVER a lookup key on its own.
  order_prefix        TEXT NOT NULL,
  order_sequence      INT  NOT NULL CHECK (order_sequence > 0),
  order_number        TEXT NOT NULL,                       -- 'POS-001'

  channel             TEXT NOT NULL CHECK (channel IN ('web', 'pos')),                           -- Δ CHECK
  fulfillment_summary TEXT NOT NULL CHECK (fulfillment_summary IN ('dine_in', 'takeaway', 'mixed')), -- Δ CHECK
  status              TEXT NOT NULL CHECK (status IN (
                        'draft', 'pending_payment', 'placed', 'preparing', 'ready',
                        'completed', 'cancelled', 'expired', 'refunded')),                       -- Δ CHECK

  table_number        TEXT,
  note                TEXT,

  subtotal            BIGINT NOT NULL DEFAULT 0 CHECK (subtotal >= 0),
  discount_total      BIGINT NOT NULL DEFAULT 0 CHECK (discount_total >= 0),
  service_charge      BIGINT NOT NULL DEFAULT 0 CHECK (service_charge >= 0),
  packaging_fee       BIGINT NOT NULL DEFAULT 0 CHECK (packaging_fee >= 0),
  tax_total           BIGINT NOT NULL DEFAULT 0 CHECK (tax_total >= 0),
  grand_total         BIGINT NOT NULL DEFAULT 0 CHECK (grand_total >= 0),
  tax_breakdown       JSONB  NOT NULL DEFAULT '{}',

  business_date       DATE NOT NULL,                       -- the shift's (§1.4)
  placed_at           TIMESTAMPTZ,
  paid_at             TIMESTAMPTZ,                         -- Δ paid is not a status (FR-ORD-3)
  created_by          UUID REFERENCES staff(id),           -- null for web orders
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- The number is unique only within its shift.
  UNIQUE (outlet_id, shift_id, order_prefix, order_sequence),
  -- Δ a counter order always has a device and a staff member
  CHECK (channel <> 'pos' OR (device_id IS NOT NULL AND created_by IS NOT NULL))
);
-- Δ Web columns (customer_id, guest_name, guest_phone, tracking_token, pickup_at)
--   are added by Web Orders.

CREATE INDEX orders_shift_status_idx ON orders (shift_id, status, created_at DESC);        -- guard, POS list
CREATE INDEX orders_outlet_business_date_idx ON orders (outlet_id, business_date DESC, created_at DESC);

CREATE TABLE order_lines (
  id                 UUID PRIMARY KEY,                     -- client-generated, retry key (FR-ADD-3)
  order_id           UUID NOT NULL REFERENCES orders(id),  -- Δ no ON DELETE CASCADE: orders are never deleted

  product_id         UUID NOT NULL REFERENCES products(id),
  product_variant_id UUID REFERENCES product_variants(id),

  -- Immutable snapshot. Reporting reads THESE, never the products table.
  product_name       TEXT   NOT NULL,
  variant_name       TEXT,
  category_id        UUID   NOT NULL REFERENCES categories(id),  -- Δ catalog-core FR-QTE-4
  category_name      TEXT   NOT NULL,                            -- Δ
  unit_price         BIGINT NOT NULL CHECK (unit_price >= 0),
  unit_cost          BIGINT,

  quantity           INT    NOT NULL CHECK (quantity > 0),
  fulfillment_type   TEXT   NOT NULL CHECK (fulfillment_type IN ('dine_in', 'takeaway')),  -- Δ CHECK

  discount_amount    BIGINT NOT NULL DEFAULT 0 CHECK (discount_amount >= 0),  -- unused until POS Controls
  discount_reason    TEXT,
  line_total         BIGINT NOT NULL CHECK (line_total >= 0),
  note               TEXT,

  status             TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'voided')),  -- Δ CHECK
  voided_by          UUID REFERENCES staff(id),
  voided_at          TIMESTAMPTZ,                          -- Δ
  void_reason        TEXT,
  created_by         UUID REFERENCES staff(id),            -- Δ who added the line (FR-ADD-4)
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Δ void fields are all set or all null
  CHECK ((status = 'voided') = (voided_at IS NOT NULL)),
  CHECK ((status = 'voided') = (void_reason IS NOT NULL))
);

CREATE INDEX order_lines_order_idx ON order_lines (order_id, created_at);

CREATE TABLE order_line_modifiers (
  id            UUID PRIMARY KEY,
  order_line_id UUID NOT NULL REFERENCES order_lines(id),  -- Δ no ON DELETE CASCADE
  modifier_id   UUID NOT NULL REFERENCES modifiers(id),
  modifier_name TEXT   NOT NULL,                           -- snapshot
  price_delta   BIGINT NOT NULL                            -- snapshot
);

CREATE INDEX order_line_modifiers_line_idx ON order_line_modifiers (order_line_id);

CREATE TABLE order_status_events (
  id          UUID PRIMARY KEY,
  order_id    UUID NOT NULL REFERENCES orders(id),
  from_status TEXT,
  to_status   TEXT NOT NULL,
  reason      TEXT,
  staff_id    UUID REFERENCES staff(id),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX order_status_events_order_idx ON order_status_events (order_id, created_at);

CREATE TABLE payments (
  id            UUID PRIMARY KEY,                          -- client-generated on POS, retry key (FR-PAY-5)
  order_id      UUID NOT NULL REFERENCES orders(id),
  method        TEXT NOT NULL CHECK (method IN ('qris_online', 'qris_static', 'card_edc')),   -- Δ CHECK
  amount        BIGINT NOT NULL CHECK (amount >= 0),
  status        TEXT NOT NULL CHECK (status IN ('pending', 'settled', 'expired', 'failed')),  -- Δ CHECK

  provider      TEXT,                                      -- web payments, unused here
  provider_ref  TEXT,
  provider_fee  BIGINT,
  qr_string     TEXT,
  qr_expires_at TIMESTAMPTZ,
  external_ref  TEXT,                                      -- RRN from EDC slip or static QRIS app

  recorded_by   UUID REFERENCES staff(id),
  settled_at    TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX payments_order_idx ON payments (order_id);
-- Δ One settled payment per order (FR-PAY-6). No split payments.
CREATE UNIQUE INDEX one_settled_payment_per_order ON payments (order_id) WHERE status = 'settled';

CREATE TABLE outbox_events (
  id             UUID PRIMARY KEY,
  aggregate_type TEXT NOT NULL,
  aggregate_id   UUID NOT NULL,
  event_type     TEXT NOT NULL,                            -- order.placed | order.updated | order.cancelled | order.paid
  payload        JSONB NOT NULL,
  published_at   TIMESTAMPTZ,
  attempts       INT NOT NULL DEFAULT 0,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Δ The worker's queue: unpublished rows, oldest first.
CREATE INDEX outbox_events_unpublished_idx ON outbox_events (created_at) WHERE published_at IS NULL;

```

`refunds` and `payment_webhook_events` are **not** created here.

**Redis:** none.

**New error codes** (`internal/domain/errors.go`): `no_open_shift`, `lines_unavailable`, `order_not_open`, `last_active_line`, `amount_mismatch`, `order_already_paid` (all 409), `order_unavailable` (503).

**New permission constants** (`internal/domain/access.go`): `PermOrderCreate`, `PermOrderVoidUnpaid`, `PermPaymentRecord`. The permission strings are already seeded.

**API:**

```
# POS (pos audience, RequireActiveDevice, RequireActingStaff)
POST   /v1/pos/orders                                        # process order
GET    /v1/pos/orders
GET    /v1/pos/orders/{order_id}
POST   /v1/pos/orders/{order_id}/lines                       # add lines
POST   /v1/pos/orders/{order_id}/lines/{line_id}/void
POST   /v1/pos/orders/{order_id}/cancel
POST   /v1/pos/orders/{order_id}/payments

# Dashboard — per outlet, read-only
GET    /v1/outlets/{outlet_id}/orders
GET    /v1/outlets/{outlet_id}/orders/{order_id}
```

Δ The overview's indicative POS routes under `/v1/orders` move under `/v1/pos`, like shifts, so the outlet comes from the device token. `/v1/orders` is left for the public web checkout. `PATCH /v1/orders/{id}/lines` becomes `POST …/lines` (add) and `POST …/lines/{line_id}/void`.

**Internal (package `internal/order`):** `handler.go`, `service.go`, `repository.go`, `totals.go`, per coding standards. It exports the `shift.OrderGuard` implementation, wired in `cmd/api/main.go`. One transaction (`postgres.InTx`) serves `shift.Current`, `catalog.Quote`, `shift.NextNumber` and the inserts; `shift.Querier` and `catalog.Querier` have the same shape.

**Docs to update when this lands:** `context/spec/data-model.md` (the DDL above), `context/spec/flows-and-api.md` (lifecycle diagram: no server draft, payment completes a POS order; POS routes).

---

## 9. Risks and Accepted Trade-offs

| Risk | Status |
| --- | --- |
| **No connection, no sale.** Until Offline Sync exists, the POS cannot process an order or record a payment offline. This is narrower than the overview's offline-first promise and shifts NFR-AV-2. | Accepted for this slice. Offline Sync is the recommended next spec for the POS. The client-generated IDs and idempotent routes here are what sync will replay. |
| **Payments are unverified claims.** A cashier can record a payment that never happened. | Accepted (overview §F). Controls are the optional reference, the void and cancel audit trail, and per-tender reconciliation once Shift Settlement exists. |
| **"Completed" means paid, not handed over.** For pay-first orders the food may still be cooking when the order is `completed`. | Accepted by decision; matches the counter flow. A kitchen display or handover step would add `preparing` / `ready` and move completion later. |
| **A cashier can void or cancel anything unpaid.** No approval, only a reason. | Accepted by decision. Every void and cancel is audited with its value and reported per staff member (§7.2). A void after the kitchen has started cooking is indistinguishable here; revisit with kitchen statuses. |
| **Paid orders cannot be corrected at all in this slice.** The paid void arrives with Refunds + Paid Void. | Accepted. Until then a mistaken paid order stays in the figures and is corrected outside the system. |
| **Zero-total orders.** An order whose lines are all free is completed by a payment of `amount = 0`, which records a tender for no money. | Accepted as the single path to `completed`. Revisit if free items become common (a `no_charge` tender is the likely answer, with POS Controls). |
| **Open bills block shift close.** A forgotten open bill keeps the shift open. | Accepted (shifts-spec §9). The `409` lists the bills; the old-open-bills signal (§7.2) surfaces them earlier. |
| **Outbox rows accumulate.** Nothing publishes or prunes them yet. | Accepted. A few rows per order; the worker spec defines publishing and retention. |
| **Retry returns the stored order without comparing the body.** A POS bug that reuses an order ID for a different ticket silently drops the second ticket's lines. | Accepted, same as shifts. The POS must generate one UUID per ticket. Body comparison would need the `Idempotency-Key` store. |
| **Charges are not modelled yet.** Adding PB1, service charge or packaging later needs rates, configuration and a calculation order. | Accepted by decision. The total columns and `tax_breakdown` exist, so no change to `orders` is needed, only to the calculation. |
| **Acting staff is asserted by the device.** `X-Acting-Staff-Id` is not proven to the server. | Accepted. Open item M3 from the 2026-11-02 scan; applies to every POS route. |
