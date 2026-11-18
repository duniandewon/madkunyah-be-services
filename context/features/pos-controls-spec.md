# POS Controls — Feature Spec

## 1. Introduction

This feature lets the POS lower or change what an open bill charges, under supervisor control. It adds:

- **line discounts**: a percentage or fixed rupiah amount off one line
- **order discounts**: a percentage or fixed rupiah amount off the whole bill, spread across its lines for reporting
- **price overrides**: a different unit price for one line, up or down
- **step-up** for all three: a cashier who lacks the permission acts with an approver who holds it, checked server side with `authz.Approver`
- an audit row for every applied, changed or removed control, with the money it moved

Controls can be sent with the lines when an order is placed or lines are added, and they can be applied, changed or removed on an open bill through dedicated routes. Once the order is paid, nothing in this spec can change it.

The counter flow this spec extends (orders-spec §1):

1. The cashier enters the order. For a discounted or re-priced item, the POS asks for a reason and, if the cashier lacks the permission, for an approver who enters their own PIN on the device (auth FR-STEP-2).
2. *Process order* sends the lines with their controls. The server prices them with `catalog.Quote`, applies the controls, checks every approval, and stores the order.
3. Until payment, a cashier can apply, change or remove a control on the open bill, under the same approval rule.
4. Payment must equal the `grand_total` after controls, as before (orders FR-PAY-3).

**Depends on:**

- Orders Core: `orders`, `order_lines`, totals (`internal/order/totals.go`), the open-bill routes and row locking (orders FR-ORD-6)
- `authz.Approver` (`internal/authz/stepup.go`), added with Shifts Core
- Auth spec FR-STEP-1..5 (step-up rules), the POS roster (auth slice 3)
- `audit_logs` with `approved_by` (migration `0002_authz`)

**Related (later specs):**

- **Offline Sync**: FR-STEP-6 (re-check approvals against roles at action time), `authz.offline_violation`
- **Refunds + Paid Void**: any change after payment
- **Reports**: the void/discount report, which reads the audit rows and line columns written here

### 1.1 Goals

- A supervisor, or a cashier with a supervisor's approval, can discount a line, discount the whole bill, or override a line's price, in one request.
- Every control records who applied it, who approved it, why, and how much it moved, in the same transaction.
- Revenue can still be aggregated from lines: each line knows its own discount and its share of the order discount.
- What was charged can be reproduced from the order's rows alone, including the price before an override.
- A retry never applies a control twice.

### 1.2 In scope

- Migration `0008_pos_controls`: control columns on `order_lines` and `orders`
- Controls on place (`POST /v1/pos/orders`) and add-lines (`POST …/lines`)
- Open-bill routes to apply, replace and remove each control
- Step-up through `authz.Approver` with `order.discount` and `order.price_override`
- Totals calculation with line discounts, order discount, allocation and rounding
- Reads (POS and dashboard) returning the controls
- Audit, outbox, error contract and metrics for the above

### 1.3 Out of scope

- **Preset or named discounts** (e.g. "Staff meal 50%") configured in the dashboard. Every discount is entered on the POS with a reason.
- **Vouchers, promo rules, loyalty** → Phase 3 (roadmap).
- **No-charge tender.** A fully discounted order is still completed by a payment of `amount = 0` (orders-spec §9).
- **Changing a paid order** → Refunds + Paid Void.
- **FR-STEP-6** offline re-check and `authz.offline_violation` → Offline Sync. Every control in this spec happens online and is checked at request time.
- **Server-side proof of the approver's PIN.** The PIN is checked on the device only (open item M3, §9).
- **The void/discount report** → Reports. This spec writes the data it needs.
- **Dashboard writes.** The dashboard reads controls but cannot apply them.
- **Limits per role** (e.g. supervisors up to 50%). Anyone with the permission can apply up to 100%.
- Discounts on modifiers alone. A line discount applies to the whole line, modifiers included.
- Tax, service charge and packaging fee, still 0 (orders §1.3).

### 1.4 Decisions carried from the project overview, earlier specs and the user

| Decision | Source |
| --- | --- |
| Discounts and price overrides need `order.discount` / `order.price_override`: supervisor, manager, owner, as seeded | Overview §C, auth FR-STEP-1, migration `0001_auth` |
| A cashier without the permission acts with an approver who enters their own PIN on the device | Auth FR-STEP-2 |
| Every step-up action records `actor_id` and `approved_by`; one approval covers one action | Auth FR-STEP-3, FR-STEP-4 |
| A staff member holding the permission is their own approver | Auth FR-STEP-5 |
| The server re-checks the approver's permission itself, never trusting the device's roster | Shifts Core (`authz.Approver`) |
| Every discount and price override is audited with who, when, why and value moved | Overview §I |
| Revenue reporting aggregates from lines, never from the order header | Overview §E |
| Prices and totals are never accepted from the client; an override is the one priced field the client sends, and only under the permission | Orders NFR-SEC-2, extended here |
| **Discounts at line and order level; the order discount is allocated across active lines** | Decided for this feature |
| **Discounts are a percentage or a fixed amount; the server computes the rupiah amount** | Decided for this feature |
| **Controls are accepted on place and add-lines and on an open bill; never after payment** | Decided for this feature |
| **No-charge tender stays out of scope** | Decided for this feature |

### 1.5 Vocabulary

| Term | Meaning |
| --- | --- |
| Control | A line discount, an order discount, or a price override |
| Gross line amount | `unit_price × quantity`, after any override, before any discount |
| Line discount | Rupiah taken off one line: `order_lines.discount_amount` |
| Order discount | Rupiah taken off the whole bill: `orders.discount_total` |
| Order discount share | The part of the order discount allocated to one line: `order_lines.order_discount_share` |
| Net line revenue | `line_total − order_discount_share`. What reporting counts as the line's revenue |
| Price override | Replacing a line's `unit_price`; `original_unit_price` keeps what `Quote` returned |
| Actor | The acting staff member (`X-Acting-Staff-Id`) |
| Approver | The staff member whose permission covers the action; the actor themselves when they hold it |

---

## 2. Actors and Access

| Actor | Client | What they do | Audience | Permission |
| --- | --- | --- | --- | --- |
| Supervisor, manager, owner | POS | Apply, change or remove a discount or override | `pos` + acting staff | `order.discount` / `order.price_override` (outlet) |
| Cashier with an approver | POS | Same, with `approved_by` naming a holder of the permission | `pos` + acting staff | The approver holds the permission at the outlet |
| Any staff with a role at the outlet | POS, dashboard | Read orders with their controls | as in orders §2 | any role at the outlet |

The routes that carry the controls keep their own permissions: placement and add-lines still need `order.create` from the actor (orders §3.8). A control adds the step-up check on top.

Role permissions are unchanged. `order.discount` and `order.price_override` are already seeded.

---

## 3. Functional Requirements

Requirement IDs are stable and referenced by the acceptance criteria in §6. Orders-spec IDs (FR-ORD-*, FR-PLC-*, …) still apply unless changed here.

### 3.1 Control objects

- **FR-CTL-1** A **discount** (line or order) is:

  ```json
  { "type": "percent", "value": 10, "reason": "Regular customer", "approved_by": "<staff id>" }
  ```

  - `type` is `percent` or `fixed`.
  - `percent`: `value` is an integer from 1 to 100.
  - `fixed`: `value` is an integer number of rupiah, from 1 to `CATALOG_PRICE_MAX`.
  - `reason` is required, 3 to 280 characters, trimmed.
  - `approved_by` is optional (FR-STP-1).
- **FR-CTL-2** A **price override** is:

  ```json
  { "unit_price": 15000, "reason": "Wrong price on the menu board", "approved_by": "<staff id>" }
  ```

  - `unit_price` is an integer from 0 to `CATALOG_PRICE_MAX`. It may be above or below the quoted price.
  - It replaces the whole `unit_price`, variant and modifier deltas included. Modifier snapshots (`order_line_modifiers.price_delta`) are kept unchanged as the record of what was chosen.
  - `reason` and `approved_by` as in FR-CTL-1.
- **FR-CTL-3** Invalid fields return `400 invalid_input` per field, like orders FR-PLC-2.
- **FR-CTL-4** A line has at most one discount and at most one override; an order has at most one order discount. Applying a control where one exists **replaces** it (FR-OPN-3).
- **FR-CTL-5** The server stores `type` and `value` as sent, and computes the rupiah amount itself (FR-TOT-*). The amount is never accepted from the client.

### 3.2 Totals

Replaces orders FR-TOT-2 and FR-TOT-3. FR-TOT-1, FR-TOT-4, FR-TOT-5 and FR-TOT-6 still apply, except that FR-TOT-6 now admits the override's `unit_price`.

- **FR-TOT-7** **Unit price.** `unit_price` is the override's `unit_price` when the line has one, else the `Quote` price. `original_unit_price` holds the `Quote` price whenever an override exists.
- **FR-TOT-8** **Line discount amount.** With gross = `unit_price × quantity`:
  - `percent`: `round_half_up(gross × value / 100)`, in integer arithmetic
  - `fixed`: `value`. It must not exceed gross, else `409 discount_exceeds_total` with `details.max` = gross
- **FR-TOT-9** `line_total = gross − discount_amount`. A 100% discount gives `line_total = 0`.
- **FR-TOT-10** `subtotal` = Σ `line_total` of active lines.
- **FR-TOT-11** **Order discount amount** (`discount_total`), from `subtotal`:
  - `percent`: `round_half_up(subtotal × value / 100)`
  - `fixed`: `min(value, subtotal)`. When the discount is applied, `value > subtotal` returns `409 discount_exceeds_total` with `details.max` = subtotal. When it is recomputed later (FR-TOT-14), it is clamped instead.
- **FR-TOT-12** `grand_total = subtotal − discount_total` (+ charges, still 0).
- **FR-TOT-13** **Allocation.** `discount_total` is spread over active lines in proportion to `line_total`, stored per line in `order_discount_share`:
  1. each line gets `floor(discount_total × line_total / subtotal)`
  2. the remaining rupiah go one each to the lines with the largest fractional remainders, ties broken by `line_no`
  3. Σ `order_discount_share` = `discount_total` exactly, and no share exceeds its `line_total`

  Voided lines and lines with `line_total = 0` get 0. With no order discount, or `subtotal = 0`, every share is 0.
- **FR-TOT-14** **Recompute.** Line discount amounts, `subtotal`, `discount_total`, allocation and `grand_total` are recomputed in the same transaction whenever a line is added or voided or any control changes. A percentage order discount follows the subtotal; a fixed one stays at its value unless clamped.
- **FR-TOT-15** **Override and fixed discount.** An override that would make gross smaller than an existing fixed line discount returns `409 discount_exceeds_total` with `details.max` = the new gross, and nothing is written. The cashier removes or lowers the discount first. A percentage discount is simply recomputed.
- **FR-TOT-16** The calculation is one pure function in `internal/order/totals.go` (extending `computeTotals`) that takes the lines' prices, quantities and controls and the order discount, and returns every line's `discount_amount`, `line_total`, `order_discount_share` and the order totals. Every write path uses it.

### 3.3 Step-up

- **FR-STP-1** Each control is checked with `authz.Approver(ctx, resolver, actor, outletID, perm, approved_by)`:

  | Control | `perm` |
  | --- | --- |
  | Line discount, order discount | `order.discount` (`PermOrderDiscount`) |
  | Price override | `order.price_override` (`PermOrderPriceOverride`) |

  If the actor holds `perm` at the outlet, they are the approver and `approved_by` is ignored (FR-STEP-5). Otherwise `approved_by` must name an active staff member who holds `perm` at the outlet, per the server's own authz view.
- **FR-STP-2** A missing, unknown, deactivated or unpermitted approver returns `403 forbidden`, and nothing is written. On placement and add-lines, one failing control rejects the whole request.
- **FR-STP-3** **Removing a control needs the same permission** as applying it. Removing a discount raises the bill, but it is still a change to a supervised decision.
- **FR-STP-4** **One approval, one action** (FR-STEP-4). Each control in a request carries its own `approved_by`. The server checks each; the POS is responsible for asking for a PIN per control. Two controls approved by the same person are two checks, not one.
- **FR-STP-5** Step-up runs before the transaction. Permission checks use the authz resolver (cached), so a request with *n* controls and *k* distinct approvers makes at most *k* resolver calls.
- **FR-STP-6** Step-up is checked only when something is applied. A retry answered with `applied: false` (FR-PLC-6, FR-ADD-3, FR-OPN-4) returns the stored order without checking approvals again.

### 3.4 Controls on place and add-lines

- **FR-PLA-1** `POST /v1/pos/orders` accepts, in addition to orders FR-PLC-1:
  - per line, optional `discount` (FR-CTL-1) and `price_override` (FR-CTL-2)
  - on the order, optional `discount` (FR-CTL-1)
- **FR-PLA-2** `POST /v1/pos/orders/{order_id}/lines` accepts per line `discount` and `price_override`. It does not accept an order `discount`; use FR-OPN-1.
- **FR-PLA-3** In the transaction, after `Quote` prices the lines, the server applies overrides, then line discounts, then the order discount, using FR-TOT-*. A `discount_exceeds_total` on any line or on the order rejects the whole request, nothing is written, and no number is used (orders NFR-AV-2). With several problems, `details` names the first line in request order (`details.line_id`) or the order.
- **FR-PLA-4** A line with an unavailable product still fails with `409 lines_unavailable` first (orders FR-PLC-5); controls are not evaluated for a rejected order.
- **FR-PLA-5** Each applied control writes one audit row (FR-CAUD-1). Outbox events are unchanged: one `order.placed` or one `order.updated` for the request.

### 3.5 Controls on an open bill

- **FR-OPN-1** Routes, all on `placed` orders of the device's outlet, with `X-Acting-Staff-Id`:

  | Route | Body | Effect |
  | --- | --- | --- |
  | `PUT /v1/pos/orders/{order_id}/discount` | discount (FR-CTL-1) | Apply or replace the order discount |
  | `DELETE /v1/pos/orders/{order_id}/discount` | `{ "approved_by": "…" }` (optional) | Remove it |
  | `PUT /v1/pos/orders/{order_id}/lines/{line_id}/discount` | discount | Apply or replace a line discount |
  | `DELETE /v1/pos/orders/{order_id}/lines/{line_id}/discount` | `{ "approved_by": "…" }` (optional) | Remove it |
  | `PUT /v1/pos/orders/{order_id}/lines/{line_id}/price` | price override (FR-CTL-2) | Apply or replace an override |
  | `DELETE /v1/pos/orders/{order_id}/lines/{line_id}/price` | `{ "approved_by": "…" }` (optional) | Restore `original_unit_price` |

  DELETE takes an optional body only for `approved_by`; no reason is required to remove a control. An empty body is accepted.
- **FR-OPN-2** Each route locks the order `FOR UPDATE` (orders FR-ORD-6), applies the change, recomputes (FR-TOT-14), and stores the line and order columns. A non-`placed` order returns `409 order_not_open` with `details.status` (orders FR-ORD-7), with no exception for retries: once paid or cancelled, the stored state is final.
- **FR-OPN-3** **Replace.** A PUT where a control exists replaces it: type, value, reason, actor, approver and time are all overwritten. The audit row keeps the previous control in `before` (FR-CAUD-1).
- **FR-OPN-4** **No-ops.** A PUT with the same `type` and `value` (or `unit_price`) as the stored control, or a DELETE where there is no control, returns `200`, `applied: false`, changes nothing and writes no audit row or event. A differing `reason` alone is not a change.
- **FR-OPN-5** A line that is `voided` returns `409 line_not_active`. A line of another order returns `404 not_found` (orders E-21).
- **FR-OPN-6** An override PUT whose `unit_price` equals `original_unit_price` (or the current `Quote` snapshot when no override exists) is treated as a DELETE of the override: the override is removed, or the request is a no-op.
- **FR-OPN-7** Each applied change writes one audit row (FR-CAUD-1) and one `order.updated` outbox event, with `line_ids` = the affected line for line routes and `[]` for the order discount.
- **FR-OPN-8** All six routes return the order (orders §3.7 shape, extended by FR-RDX-1) with `applied`. Success is `200` for every applied change and every no-op.
- **FR-OPN-9** Payment is unchanged: `amount` must equal the current `grand_total` (orders FR-PAY-3). A POS that applied a control and lost the response sees `409 amount_mismatch` and refreshes.

### 3.6 Interaction with voids and cancellation

- **FR-INT-1** Voiding a line removes its `line_total` and its order discount share from the bill; the order discount is recomputed and reallocated over the remaining lines (FR-TOT-14). The void audit row keeps `value_delta = −line_total` (orders FR-OAUD-1), net of the line's own discount. The change in `discount_total` from the recompute writes no extra audit row.
- **FR-INT-2** A voided line keeps its control columns as they were at the void, so the record shows what was voided.
- **FR-INT-3** Cancelling keeps every control column and stored total as they were (orders FR-CAN-2). The cancel audit row keeps `value_delta = −grand_total`.
- **FR-INT-4** Adding lines recomputes a percentage order discount over the new subtotal. Existing line discounts and overrides are unchanged.

### 3.7 Reads

- **FR-RDX-1** Single and list reads (POS and dashboard) gain:

  On the order header:
  ```json
  "discount": {
    "type": "percent", "value": 10, "amount": 5400, "reason": "Regular customer",
    "by": { "id": "…", "name": "Rina" }, "approved_by": { "id": "…", "name": "Dewi" }, "at": "…"
  }
  ```
  `null` when there is none. `discount_total` stays as today and equals `discount.amount`.

  On each line (single reads only):
  ```json
  "discount": { "type": "fixed", "value": 2000, "amount": 2000, "reason": "…", "by": {…}, "approved_by": {…}, "at": "…" },
  "price_override": { "original_unit_price": 27000, "reason": "…", "by": {…}, "approved_by": {…}, "at": "…" },
  "order_discount_share": 540
  ```
  `discount` and `price_override` are `null` when absent. `discount_amount` stays as today.
- **FR-RDX-2** `unit_cost` is still never returned (orders FR-RD-6).
- **FR-RDX-3** Single reads keep a fixed number of queries; staff names for `by` and `approved_by` are loaded with the existing staff lookup, not per line.

### 3.8 Authorization summary

| Route | Audience | Requirement | No role at outlet |
| --- | --- | --- | --- |
| `POST /v1/pos/orders` with controls | `pos` + acting staff | `order.create`, plus FR-STP-1 per control | `403` |
| `POST /v1/pos/orders/{order_id}/lines` with controls | `pos` + acting staff | `order.create`, plus FR-STP-1 per control | `403` |
| `PUT`/`DELETE /v1/pos/orders/{order_id}/discount` | `pos` + acting staff | `order.discount` by actor or approver | `403` |
| `PUT`/`DELETE …/lines/{line_id}/discount` | `pos` + acting staff | `order.discount` by actor or approver | `403` |
| `PUT`/`DELETE …/lines/{line_id}/price` | `pos` + acting staff | `order.price_override` by actor or approver | `403` |

Every POS route also runs `RequireActiveDevice`. An order of another outlet behaves as unknown (`404`). The acting staff member must have a role at the outlet even when an approver covers the permission; `authz.Approver` is called only after that check.

### 3.9 Audit

- **FR-CAUD-1** Each applied control writes one `audit_logs` row in the **same transaction**, with `outlet_id`, `actor_type = staff`, `actor_id` = the actor, `approved_by` = the approver from FR-STP-1, `entity_type = order`, `entity_id` = the order ID, and `reason` = the control's reason (null on removal):

  | Action | When | `value_delta` | `before` / `after` |
  | --- | --- | --- | --- |
  | `line.discount` | Line discount applied or replaced | change in `grand_total` caused by the action (≤ 0 when applied; may be positive on a replace that lowers it) | `line_id`, `type`, `value`, `amount`; `after` adds `order_number`, `product_name` |
  | `line.discount_removed` | Line discount removed | change in `grand_total` (≥ 0) | `before` as above; `after` null |
  | `price.override` | Override applied or replaced | change in `grand_total` (either sign) | `line_id`, `unit_price`, `original_unit_price`, `quantity`; `after` adds `order_number`, `product_name` |
  | `price.override_removed` | Override removed | change in `grand_total` | `before` as above; `after` null |
  | `order.discount` | Order discount applied or replaced | change in `grand_total` | `type`, `value`, `amount`; `after` adds `order_number` |
  | `order.discount_removed` | Order discount removed | change in `grand_total` | `before` as above; `after` null |

  On placement and add-lines, `before` is null and `value_delta` is the control's own effect measured in application order (FR-PLA-3): overrides against the quoted price, line discounts against the gross, the order discount against the subtotal.
- **FR-CAUD-2** `value_delta` is the whole change in `grand_total` caused by the action, including any reallocation of a percentage order discount. A line discount of 2,000 on an order with a 10% order discount moves `grand_total` by −1,800, and that is what is recorded.
- **FR-CAUD-3** Recomputations caused by other actions (FR-INT-1, FR-INT-4) write no audit row for the control. The stored columns are the truth; the audit row records the decision.
- **FR-CAUD-4** No-ops and rejected requests write no audit row or outbox event (orders FR-OAUD-3).

---

## 4. Non-Functional Requirements

### 4.1 Security

- **NFR-SEC-1** The only client-supplied money values are discount `value` and override `unit_price`, each behind its permission. Computed amounts and totals are never accepted.
- **NFR-SEC-2** `approved_by` is checked against the server's authz view (FR-STP-1), never the device's roster.
- **NFR-SEC-3** Body caps: placement and add-lines keep 64 KB; the six control routes 4 KB.
- **NFR-SEC-4** `reason` is stored as plain text and never interpreted.

### 4.2 Performance

- **NFR-PERF-1** Placement of a 20-line order with a control on every line: p95 ≤ 150 ms (unchanged target). Step-up adds at most one resolver call per distinct approver (FR-STP-5).
- **NFR-PERF-2** Control routes: p95 ≤ 100 ms. Updating the order's lines after a recompute is one batched statement, not one per line.

### 4.3 Availability and resilience

- **NFR-AV-1** No new Redis use beyond the authz cache, which falls back to Postgres as today.

  | Failure | Behaviour |
  | --- | --- |
  | Postgres down | `503 order_unavailable` |
  | Authz resolver cannot reach Postgres or Redis | `503 order_unavailable`; no control is applied unchecked |
  | Commit outcome uncertain | `503 order_unavailable`. A retried PUT/DELETE with the same body is a no-op if the first committed (FR-OPN-4), so it is safe |

### 4.4 Operability and testability

- **NFR-OPS-1** Totals, rounding and allocation are one pure function with a table-driven unit test (FR-TOT-16).
- **NFR-OPS-2** Integration tests run against `TEST_DATABASE_URL`, as in Orders Core.
- **NFR-OPS-3** OpenAPI and the metrics in §7 follow once those exist (deferred project-wide).

---

## 5. Edge Cases and Error Handling

### 5.1 Error contract

Uses the existing envelope. Codes used here, in addition to the orders-spec codes that still apply:

| HTTP | `code` | When | Notes |
| --- | --- | --- | --- |
| 400 | `invalid_input` | Bad type, value out of range, missing or short reason, bad `approved_by` UUID | Per field |
| 403 | `forbidden` | Actor lacks the permission and no valid approver | Nothing written |
| 404 | `not_found` | Unknown order or line, line of another order, order at another outlet | |
| 409 | `order_not_open` | Control on a completed or cancelled order | `details.status` |
| 409 | `line_not_active` | **New.** Control on a voided line | |
| 409 | `discount_exceeds_total` | **New.** Fixed discount above the gross or subtotal, or an override below a fixed line discount | `details.max`; `details.line_id` when on a line |
| 413 | `payload_too_large` | Body over the cap | |
| 503 | `order_unavailable` | Postgres or authz unreachable, commit uncertain | |

### 5.2 Edge cases

**Discounts**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-1 | 10% off a 27,000 × 2 line | `discount_amount = 5,400`, `line_total = 48,600` |
| E-2 | 15% off 9,950 (1,492.5) | Rounds half up to 1,493 |
| E-3 | 100% off every line | `grand_total = 0`; completed by a payment with `amount = 0` (orders §9) |
| E-4 | Fixed 30,000 off a 27,000 line | `409 discount_exceeds_total`, `details.max = 27,000` |
| E-5 | Line discount replaced, 10% → 20% | One `line.discount` row; `before` holds 10%, `value_delta` the further drop |
| E-6 | Same discount PUT twice (lost response) | Second returns `200`, `applied: false`, no second audit row |
| E-7 | Same type and value, new reason | `applied: false`; the stored reason is kept |
| E-8 | Remove a discount the order never had | `200`, `applied: false` |
| E-9 | 10% order discount on three lines of 10,000 / 10,000 / 10,001 (subtotal 30,001 → 3,000) | Floors 999 / 999 / 1,000 (Σ 2,998); the two remaining rupiah go to the two lines with the largest remainders (0.97 each), giving 1,000 / 1,000 / 1,000, Σ = 3,000 |
| E-10 | Fixed 20,000 order discount, then a void drops the subtotal to 15,000 | `discount_total` clamps to 15,000; `grand_total = 0`; no extra audit row (FR-CAUD-3) |
| E-11 | Percentage order discount, then a line is added | `discount_total` grows with the subtotal; no audit row |
| E-12 | Order discount sent on add-lines | `400 invalid_input`; use `PUT …/discount` |

**Price overrides**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-13 | Override 27,000 → 20,000 on a line of 2 | `unit_price = 20,000`, `original_unit_price = 27,000`, `value_delta = −14,000` |
| E-14 | Override above the quoted price | Accepted; positive `value_delta` |
| E-15 | Override to 0 | Accepted; `line_total = 0` |
| E-16 | Override then a 10% discount | Discount on the overridden gross |
| E-17 | Fixed 5,000 discount, then override to 4,000 × 1 | `409 discount_exceeds_total`, `details.max = 4,000`; nothing changes |
| E-18 | Override PUT with the original price | Treated as removing the override (FR-OPN-6) |
| E-19 | Override removed on a line with a percentage discount | Discount recomputed on the quoted price |
| E-20 | Catalog price changes after an override | Nothing changes; snapshots are never re-priced (orders FR-TOT-5) |

**Step-up**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-21 | Supervisor applies a discount without `approved_by` | Allowed; approver = supervisor |
| E-22 | Cashier applies a discount with a supervisor as `approved_by` | Allowed; `actor_id` = cashier, `approved_by` = supervisor, both on the line columns and the audit row |
| E-23 | Cashier without `approved_by` | `403 forbidden` |
| E-24 | Cashier names another cashier as approver | `403 forbidden` |
| E-25 | Approver demoted or deactivated since the device's last roster sync | `403 forbidden`. The POS accepted the PIN locally; the server is the authority online |
| E-26 | Approver holds `order.discount` at another outlet only | `403 forbidden` |
| E-27 | Placement with three discounted lines, one approval invalid | `403 forbidden`, nothing written, no number used |
| E-28 | Cashier removes a discount a supervisor applied | Needs `order.discount` via step-up (FR-STP-3) |

**Concurrency and lifecycle**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-29 | A discount and a payment race | The row lock serialises them. If the discount wins, the payment gets `409 amount_mismatch`. If the payment wins, the discount gets `409 order_not_open` |
| E-30 | Discount on a paid order | `409 order_not_open` |
| E-31 | Discount on a voided line | `409 line_not_active` |
| E-32 | Discounted line voided | Void `value_delta = −line_total` (net); order discount reallocated; line keeps its control columns |
| E-33 | Discounted order cancelled | Controls and totals kept; cancel `value_delta = −grand_total` |
| E-34 | Placement retried after a lost response with an approver since demoted | `200`, `applied: false`, the stored order; no re-check (FR-STP-6) |

---

## 6. Acceptance Criteria

Each criterion is an automated integration test unless marked *(manual)*.

### Totals

- **AC-1** *(FR-TOT-7..16)* A unit test table covers: percentage and fixed line discounts, half-up rounding, 100% discount, override up, down and to 0, override with a percentage discount, percentage and fixed order discounts, allocation with remainders and ties (Σ shares = `discount_total`, no share over its `line_total`), voided and zero lines excluded, and a fixed order discount clamped after the subtotal drops.

### On place and add-lines

- **AC-2** *(FR-PLA-1, FR-PLA-3, FR-CAUD-1)* A supervisor places an order with a line override, a line percentage discount and a fixed order discount: `201`; line and order columns hold the controls with `by` = `approved_by` = the supervisor; totals match AC-1's function; three audit rows with the right actions and `value_delta`; one `order.placed` outbox row.
- **AC-3** *(FR-STP-1, FR-STP-4)* A cashier places an order with two discounted lines, each with a supervisor's `approved_by`: `201`, `actor_id` = cashier and `approved_by` = supervisor on both audit rows.
- **AC-4** *(FR-STP-2, E-27)* The same placement with one `approved_by` naming a cashier returns `403`; nothing is written and the next order gets the next number without a gap.
- **AC-5** *(FR-PLA-3, E-4)* A placement with a fixed line discount above the gross returns `409 discount_exceeds_total` with `details.max` and `details.line_id`; nothing is written.
- **AC-6** *(FR-PLA-2, E-12)* Add-lines with a line discount applies it and writes `line.discount`; add-lines with an order `discount` returns `400`.
- **AC-7** *(FR-STP-6)* Retrying a placement with controls returns `200`, `applied: false`, and writes no audit row, even after the approver's role is revoked.

### On an open bill

- **AC-8** *(FR-OPN-1..3, FR-OPN-7)* Apply, replace and remove each control (order discount, line discount, override) on an open bill: each returns `200`, `applied: true`; columns and totals are updated; one audit row per change with `before`/`after` and `value_delta`; one `order.updated` with the right `line_ids`.
- **AC-9** *(FR-OPN-4, E-6..8)* An identical PUT, a PUT differing only in reason, and a DELETE of nothing each return `200`, `applied: false`, and write no audit or outbox row.
- **AC-10** *(FR-OPN-6)* An override PUT with the original price removes the override and writes `price.override_removed`.
- **AC-11** *(FR-TOT-15, E-17)* An override below an existing fixed line discount returns `409 discount_exceeds_total` and changes nothing.
- **AC-12** *(FR-OPN-2, FR-OPN-5)* On a completed and on a cancelled order, each control route returns `409 order_not_open`. On a voided line, the line routes return `409 line_not_active`.
- **AC-13** *(FR-STP-1..3, E-22..26, E-28)* A step-up table on the open-bill routes: supervisor alone allowed; cashier with a supervisor approver allowed; cashier alone, cashier approver, approver from another outlet, deactivated approver each `403` with nothing written; removal follows the same rule.

### Interaction

- **AC-14** *(FR-INT-1, FR-INT-2, E-10, E-32)* Voiding a discounted line on an order with a percentage order discount reallocates the shares over the remaining lines, keeps the voided line's control columns, writes the void audit row with `−line_total`, and no control audit row. With a fixed order discount above the new subtotal, `discount_total` is clamped.
- **AC-15** *(FR-INT-4)* Adding a line to an order with a percentage order discount raises `discount_total` and reallocates; no control audit row.
- **AC-16** *(FR-OPN-9, E-29)* A control PUT and a payment racing on one order end in one of the two outcomes of E-29, never a completed order whose payment differs from its `grand_total`.
- **AC-17** *(FR-INT-3)* Cancelling a discounted order keeps every control column and total.

### Reads and access

- **AC-18** *(FR-RDX-1..3)* A single read returns the order `discount`, each line's `discount`, `price_override` and `order_discount_share`, with staff names, and no `unit_cost`; a list read returns the order `discount`. The query count of a single read does not grow with the line count.
- **AC-19** *(§3.8)* A route walk on the real router covers the six new routes: audience, `RequireActiveDevice`, acting staff, `403` without permission or approver, `404` for another outlet's order, `401 device_revoked` for a revoked device.
- **AC-20** *(NFR-AV-1)* With the commit reported uncertain, a control PUT returns `503 order_unavailable`, and a retry of the same PUT returns the order with the control applied exactly once.

---

## 7. Metrics

Prometheus is not wired in yet. Until then these can be read from `orders`, `order_lines` and `audit_logs`.

### 7.1 Health and experience

| Metric | Definition | Target / alert |
| --- | --- | --- |
| Control route latency | p95 of the six control routes | ≤ 100 ms |
| Step-up rejections | `403` on controls, per outlet | A rise means the POS roster is stale or a cashier is probing |
| Exceeds-total rejections | `409 discount_exceeds_total` | A steady rate means the POS lets staff enter impossible values |

### 7.2 Control signals

These feed the void/discount report (overview §F: the main fraud control without cash).

| Metric | Definition | Alert |
| --- | --- | --- |
| Discount value | Σ `−value_delta` of `line.discount` and `order.discount`, per actor, approver, outlet and business date | Outliers per staff member |
| Discount rate | Discount value / gross sales, per outlet and shift | Above a threshold set by the owner |
| Override value | Σ `value_delta` of `price.override`, per actor and approver | Any downward trend per person |
| Self-approved controls | Share of controls where actor = approver | Expected for supervisors; tracked per person |
| Approvals per approver | Count per approver per shift (auth §7) | Outliers by approver |
| 100% discounts | Lines or orders discounted to 0 | Every one reviewed |
| Removed controls | `*_removed` rows per actor | A control applied and removed repeatedly on one order |

---

## 8. Data Model — Migration `0008_pos_controls`

```sql
ALTER TABLE order_lines
  ADD COLUMN discount_type              TEXT CHECK (discount_type IN ('percent', 'fixed')),
  ADD COLUMN discount_value             BIGINT CHECK (discount_value > 0),
  ADD COLUMN discounted_by              UUID REFERENCES staff(id),
  ADD COLUMN discount_approved_by       UUID REFERENCES staff(id),
  ADD COLUMN discounted_at              TIMESTAMPTZ,
  ADD COLUMN order_discount_share       BIGINT NOT NULL DEFAULT 0 CHECK (order_discount_share >= 0),
  ADD COLUMN original_unit_price        BIGINT CHECK (original_unit_price >= 0),
  ADD COLUMN price_override_reason      TEXT,
  ADD COLUMN price_overridden_by        UUID REFERENCES staff(id),
  ADD COLUMN price_override_approved_by UUID REFERENCES staff(id),
  ADD COLUMN price_overridden_at        TIMESTAMPTZ,

  -- discount fields all set or all null; discount_reason (0007) joins the group
  ADD CONSTRAINT order_lines_discount_all_or_none CHECK (
    (discount_type IS NULL) = (discount_value IS NULL) AND
    (discount_type IS NULL) = (discount_reason IS NULL) AND
    (discount_type IS NULL) = (discounted_by IS NULL) AND
    (discount_type IS NULL) = (discount_approved_by IS NULL) AND
    (discount_type IS NULL) = (discounted_at IS NULL)),
  ADD CONSTRAINT order_lines_discount_percent_range CHECK (
    discount_type IS DISTINCT FROM 'percent' OR discount_value <= 100),
  ADD CONSTRAINT order_lines_no_amount_without_discount CHECK (
    discount_type IS NOT NULL OR discount_amount = 0),
  ADD CONSTRAINT order_lines_share_within_total CHECK (order_discount_share <= line_total),

  -- override fields all set or all null
  ADD CONSTRAINT order_lines_override_all_or_none CHECK (
    (original_unit_price IS NULL) = (price_override_reason IS NULL) AND
    (original_unit_price IS NULL) = (price_overridden_by IS NULL) AND
    (original_unit_price IS NULL) = (price_override_approved_by IS NULL) AND
    (original_unit_price IS NULL) = (price_overridden_at IS NULL));

ALTER TABLE orders
  ADD COLUMN discount_type        TEXT CHECK (discount_type IN ('percent', 'fixed')),
  ADD COLUMN discount_value       BIGINT CHECK (discount_value > 0),
  ADD COLUMN discount_reason      TEXT,
  ADD COLUMN discounted_by        UUID REFERENCES staff(id),
  ADD COLUMN discount_approved_by UUID REFERENCES staff(id),
  ADD COLUMN discounted_at        TIMESTAMPTZ,

  ADD CONSTRAINT orders_discount_all_or_none CHECK (
    (discount_type IS NULL) = (discount_value IS NULL) AND
    (discount_type IS NULL) = (discount_reason IS NULL) AND
    (discount_type IS NULL) = (discounted_by IS NULL) AND
    (discount_type IS NULL) = (discount_approved_by IS NULL) AND
    (discount_type IS NULL) = (discounted_at IS NULL)),
  ADD CONSTRAINT orders_discount_percent_range CHECK (
    discount_type IS DISTINCT FROM 'percent' OR discount_value <= 100),
  ADD CONSTRAINT orders_no_total_without_discount CHECK (
    discount_type IS NOT NULL OR discount_total = 0);
```

The existing columns `order_lines.discount_amount`, `order_lines.discount_reason` and `orders.discount_total` (migration `0007_orders`) now carry data; their meaning is unchanged. A fixed order discount clamped by FR-TOT-11 keeps its `discount_value` with a smaller `discount_total`, so `discount_total ≤ discount_value` is not a constraint.

No new index: the void/discount report reads `audit_logs` by `(outlet_id, created_at)`, which is already indexed.

**Redis:** none.

**New error codes** (`internal/domain/errors.go`): `line_not_active`, `discount_exceeds_total` (both 409).

**New permission constants** (`internal/domain/access.go`): `PermOrderDiscount`, `PermOrderPriceOverride`. The permission strings are already seeded.

**API:**

```
# POS (pos audience, RequireActiveDevice, RequireActingStaff)
POST   /v1/pos/orders                                        # + line discount / price_override, order discount
POST   /v1/pos/orders/{order_id}/lines                       # + line discount / price_override
PUT    /v1/pos/orders/{order_id}/discount
DELETE /v1/pos/orders/{order_id}/discount
PUT    /v1/pos/orders/{order_id}/lines/{line_id}/discount
DELETE /v1/pos/orders/{order_id}/lines/{line_id}/discount
PUT    /v1/pos/orders/{order_id}/lines/{line_id}/price
DELETE /v1/pos/orders/{order_id}/lines/{line_id}/price

# Dashboard reads (unchanged routes, extended shape)
GET    /v1/outlets/{outlet_id}/orders
GET    /v1/outlets/{outlet_id}/orders/{order_id}
```

**Internal (package `internal/order`):** the controls live in `internal/order` with the open-bill code (`service.go`, `repository_change.go`); the pricing and allocation in `totals.go`. Step-up uses `authz.Approver` with the resolver already injected for permission checks. No new package.

**Docs to update when this lands:** `context/spec/data-model.md` (the DDL above; drop the "unused until POS Controls" comment), `context/spec/flows-and-api.md` (the six POS routes).

---

## 9. Risks and Accepted Trade-offs

| Risk | Status |
| --- | --- |
| **The approver's PIN is not proven to the server.** The device asserts `approved_by` after a local PIN check; a tampered POS could name any supervisor. | Accepted for now, same as `X-Acting-Staff-Id` (open item M3). The server does check that the named approver holds the permission. Closing M3 (a signed approval from the device, or a server-side PIN check) covers both. |
| **No limit on discount size.** Anyone with `order.discount` can give 100%. | Accepted by decision. Every control is audited with its value and approver, and 100% discounts are a review signal (§7.2). Per-role limits can be added later as configuration. |
| **Self-approval.** Supervisors approve their own discounts, so a single person can discount and take payment. | Accepted (FR-STEP-5). Self-approved controls are reported per person (§7.2). |
| **Audited amount vs final amount.** A percentage order discount grows or shrinks as lines change, and a fixed one can be clamped, without a new audit row. The audit row records the decision; the order's columns record the final money. | Accepted. Reports must read the final discount from `orders.discount_total` and `order_lines`, and use audit rows for attribution. |
| **Override bypasses the catalog.** A wrong override persists only on that line, but a cashier with step-up could re-price freely. | Accepted. Overrides are audited with original and new price, per person (§7.2). Per-product override bans are out of scope. |
| **Rounding favours the customer on halves.** Half-up rounding of percentage discounts can give up to 1 rupiah more discount per line or order. | Accepted; immaterial. Allocation keeps the sum exact. |
| **Removing a control needs the permission.** A cashier cannot undo a supervisor's mistaken discount alone. | Accepted by design (FR-STP-3); every change to a supervised decision is supervised. |
| **Zero-total orders still record a tender.** A fully discounted order is completed with `amount = 0` on `qris_static` or `card_edc`. | Accepted by decision (§1.3). A `no_charge` tender remains the likely fix. |
| **Offline approvals.** Not possible until Offline Sync; online, a stale device roster can show an approver the server then rejects. | Accepted. The `403` is the correct online outcome; FR-STEP-6 handles the offline case later. |
