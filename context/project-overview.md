# MadKunyah Backend Service — Project Specifications

🍽️ **Unified backend for a multi-outlet Food & Beverage business** — one API serving the online shop, the in-store POS, and the owner dashboard.

**Cashless. Passwordless. Offline-tolerant.**

---

## 📌 Problem (Core Idea)

The business currently runs on disconnected tools:

- Online orders arrive in one place, counter orders in another
- Sales figures have to be manually stitched together across outlets
- Product and price changes must be re-entered per channel
- Static QRIS and card takings are reconciled by hand at close
- No single record of who voided, discounted, or refunded what

➡️ **One backend owns the catalog, the orders, the money, and the numbers. Every client is a thin view over the same truth.**

---

## 🧑‍💻 Users

|Persona|Client|Needs|
|---|---|---|
|Customer (guest)|Online shop|Order without signing up, pay by QRIS, track order status|
|Customer (account)|Online shop|OTP login, saved profile, order history, faster repeat ordering|
|Cashier|POS|Fast ticket entry, works offline, per-item dine-in/takeaway|
|Shift supervisor|POS|Voids after payment, price overrides, open/close shift, settlement check|
|Kitchen staff|POS / KDS|See what to cook, what to plate, what to box|
|Outlet manager|Dashboard|Outlet sales, staff, stock levels, variance reports|
|Business owner|Dashboard|Cross-outlet performance, margins, void/discount oversight|

---

## 🎯 Scope Decisions

Settled before schema design, because each one touches most tables:

|Decision|Choice|
|---|---|
|Outlets|**Multi-outlet from day one.** Every operational table carries `outlet_id`.|
|Terminals|**One POS device per outlet.** Order prefix is a flat `POS`; the prefix/sequence structure still allows `POS1`/`POS2` later without migration.|
|Authentication|**Passwordless — OTP over WhatsApp or email.** No password hashes stored for staff or customers.|
|Sessions|**JWT access + refresh.** Refresh tokens live in Redis; OTP codes live in Redis with TTL.|
|Cash|**No cash accepted.** Cashiers hold no physical money. Tenders are online QRIS, static QRIS, and card via EDC.|
|Inventory|**Item-level tracking now, recipe-ready schema.** A join table between products and stock items lets ingredient-level COGS slot in later.|
|Accounts|**Guest checkout is the default path**; optional customer accounts layered on top.|
|Loyalty / promos|**Out of v1.** Schema leaves room; no rules engine built yet.|
|Kitchen display|**Out of v1** as a separate client. The order event stream is built so a KDS can be added without backend changes.|
|Delivery|**Out of v1.** Dine-in, takeaway, and pickup only.|
|Split payments|**Not supported.** One payment per order; the `payments` table stays 1-to-many so this never needs a migration on live financial data.|

---

## ✨ Core Features

### A) Authentication — Passwordless

No password is ever stored. Both populations authenticate by one-time code delivered over WhatsApp (primary) or email (fallback).

**Flow**

1. `POST /auth/otp/request` with an identifier (phone or email)
2. Server generates a 6-digit code, stores an **HMAC of the code** in Redis with a 5-minute TTL, dispatches it via the notifier
3. `POST /auth/otp/verify` with identifier + code
4. On success: OTP key deleted, access token + refresh token returned

**Why the code is hashed, not stored raw:** Redis is a live cache that gets dumped, replicated, and inspected during debugging. A raw OTP in that dump is a working credential for its whole TTL window. Hashing costs nothing here.

**Abuse controls — mandatory, because every OTP costs real money on the WhatsApp API:**

|Control|Rule|
|---|---|
|Resend cooldown|60 seconds per identifier|
|Daily quota|Per identifier and per IP|
|Attempt limit|5 wrong codes → OTP invalidated, new request required|
|Single use|Code deleted on successful verification|
|Enumeration defence|Identical response and timing whether or not the identifier exists|
|Staff scope|Staff identifiers must already exist — no OTP is sent to unknown numbers, but the response is still generic|

**⚠️ The POS conflict.** OTP-only login is incompatible with an offline-first POS: no internet means no WhatsApp, which means the cashier cannot open the till. The resolution:

- OTP authenticates the **device once**, at enrollment, producing a long-lived device-bound refresh token
- Day-to-day, the cashier unlocks the already-authenticated device session with a **local PIN**
- That PIN is a _local session unlock_, not a server credential — it never authenticates to the API and grants nothing on a different device

This is technically still a stored secret, so it is stored as a hash and treated as low-trust: it gates who can act on an already-trusted device, and every action is still attributed and audited. If a device is lost, revoking its refresh-token family kills it server-side regardless of the PIN.

### B) Sessions — JWT + Redis

|Token|Lifetime|Storage|
|---|---|---|
|Access|15 minutes|Stateless JWT, not stored|
|Refresh (customer)|30 days|Redis, hashed|
|Refresh (staff, dashboard)|12 hours|Redis, hashed|
|Refresh (POS device)|90 days, sliding|Redis, hashed, device-bound|

**Rotation with reuse detection.** Every refresh issues a new token and invalidates the old one. If a token that has already been rotated is presented again, that is a theft signal — the entire token _family_ is revoked and the user must re-authenticate. Tracking a `family_id` per login is what makes this possible.

**Separate audiences.** Staff and customer tokens carry different `aud` claims and are validated by different middleware. A customer token must never be accepted on a staff route, even by accident.

**Offline token expiry.** A POS working offline will hold an expired access token. This is fine: the local queue holds unsent orders, and refresh happens when connectivity returns, before the sync batch is replayed. Orders are never blocked on token state — only their _sync_ is.

**Redis is now a hard dependency for auth.** Enable AOF persistence; a Redis flush logs out every user on every device. Refresh tokens are the one thing in Redis whose loss is user-visible.

```
otp:{purpose}:{identifier}      → {code_hmac, attempts, sent_at}     TTL 5m
otp:cooldown:{identifier}       → 1                                  TTL 60s
otp:quota:{identifier}:{date}   → counter                            TTL 24h
otp:quota:ip:{ip}               → counter                            TTL 1h
refresh:{token_hash}            → {subject_id, subject_type,
                                   device_id, family_id, issued_at}  TTL per table
refresh:family:{family_id}      → SET of token hashes
revoked:{jti}                   → 1                                  TTL 15m
idem:{scope}:{key}              → persisted response                 TTL 24h
```

### C) Access Control

**Staff** — created by admins, never self-signup. Roles are scoped per outlet, so one person can be a cashier at Outlet A and a manager at Outlet B.

|Role|Representative permissions|
|---|---|
|Cashier|Create orders, record payment, void **before** payment|
|Supervisor|Void after payment, apply discounts, price override, open/close shift|
|Outlet Manager|Manage outlet staff, stock, product availability, view outlet reports|
|Owner / Admin|All outlets, cost prices, margins, audit logs, global catalog|

**Customers** — OTP signup on first verify; no separate registration step. Guest orders capture a phone number without creating an account, so past orders can be linked retroactively if that customer later authenticates with the same number.

### D) Catalog

- Categories, products, and **variants** (size, temperature)
- **Modifier groups** with required/optional flags and min/max selections (sugar level, ice level, paid add-ons)
- **Per-outlet, per-channel pricing** — online prices commonly differ from counter prices to absorb payment fees
- Scheduled availability (breakfast menu until 11:00)
- Real-time sold-out toggle from POS, respected immediately by the online shop
- Channel visibility flags — some items are POS-only

### E) Orders

One order model across all channels. `channel` and per-line `fulfillment_type` are attributes, never separate tables.

**Per-line fulfillment.** The most important structural rule in this system: dine-in vs takeaway lives on the **line**, not the order. The header carries a derived `dine_in | takeaway | mixed` value for filtering only.

Consequences, all designed in:

- Two of the same product with different fulfillment are **two lines**, not one with qty 2
- Service charge is computed on the dine-in subtotal, not the order total
- Packaging fees attach to takeaway lines
- Kitchen tickets group by fulfillment type — plating and boxing are different actions
- All revenue reporting aggregates **from lines**, never from an order-level type column

**Order numbering.** Human-readable label, never an identity.

- Internal PK is a client-generated UUID (required for offline POS)
- `order_number` is a display string: `WEB-001`, `POS-001`
- Sequence **resets at the start of each shift**
- Uniqueness is `(outlet_id, shift_id, prefix, sequence)` — the number alone is _not_ unique
- Allocated at placement, not at draft creation, so abandoned tickets leave no gaps
- Allocation is a single atomic `UPDATE ... RETURNING` keyed by order UUID, so retries return the same number

With one terminal per outlet the cross-register collision problem disappears — but the prefix column stays, so adding a second device later means new prefixes rather than a schema change.

**Web orders attach to the outlet's currently-open shift.** Orders placed outside operating hours queue to the next shift. This keeps every shift's settlement covering all revenue in that window rather than counter revenue only.

### F) Payments — Cashless

**Online — Midtrans dynamic QRIS**

- Order sits in `pending_payment` and **does not reach the kitchen** until the webhook confirms
- The client's success redirect is never trusted
- Webhooks are signature-verified, idempotent, and tolerate out-of-order arrival (`settlement` can land before `pending`)
- A scheduled job expires unpaid orders at QRIS expiry and releases held stock
- Gateway fee recorded per transaction, so gross sales and actual payout can be reconciled

**In-store — POS as a recording tool**

Two tenders, both settled outside the system: `qris_static` (printed QR) and `card_edc`. The cashier records what happened; nothing verifies it.

**Removing cash changes the control model, and not entirely for the better.** With a drawer, a physical count was ground truth — the money either was there or it wasn't. Now _every_ in-store tender is an unverified claim, and there is no count to check it against on the day. The controls that replace it:

|Tender|System expects|Staff enters at close|Reconciled against|
|---|---|---|---|
|Static QRIS|Recorded QRIS sales|Merchant app total|QRIS settlement statement|
|Card (EDC)|Recorded card sales|EDC batch settlement slip|Acquirer statement|
|Online QRIS|Settled webhook total|— (system-verified)|Midtrans payout report|

- Variance logged per tender, mandatory note above a configurable threshold
- **Capture the RRN / reference** on static QRIS and EDC wherever the cashier can read it — without it, month-end matching against the bank statement is guesswork
- **Void and discount reporting becomes the primary fraud control**, since the classic drawer-shortage signal no longer exists

What you gain in exchange: no float, no bank drops, no drawer counts, no cash-in-transit risk, and a materially simpler shift close.

**Refunds.** `refund_method` is modelled independently of `payment_method` — QRIS refunds are frequently not programmatic and end up as a manual bank transfer with a reference entered by staff. With cash gone, an on-the-spot refund is no longer possible at all, so the manual-transfer path is not an edge case; it is the main path. Build the approval and reference capture properly.

### G) Shifts & Settlement

- Open/close shift per outlet, no opening float
- Order sequence counters reset on shift open
- Per-tender expected vs actual at close, with variance and notes
- Shift report: sales, voids, discounts, refunds, per-employee attribution
- Shift close is a **settlement check**, not a cash-up

### H) Inventory (item-level, recipe-ready)

The hinge is a `product_stock_links` join table.

- **Today:** one product → one stock item, quantity 1. Selling a burger decrements one burger.
- **Later:** one product → many stock items with real quantities. Selling a burger decrements 200g beef, one bun, one box.

Nothing about the movement ledger, the deduction job, or the reporting changes when that switch happens — only the rows in the join table.

- Per-outlet stock levels
- Append-only `stock_movements` ledger (sale, restock, waste, transfer, adjustment)
- Low-stock thresholds and alerts
- Optional stock-blocking on sale, per item

### I) Reporting

Served from **pre-aggregated read models**, never from live transactional tables.

- Sales by arbitrary date range, outlet, channel, product, category, employee, payment method, hour of day
- Dine-in vs takeaway split, computed from lines
- **Void and discount reports** — now the primary fraud signal
- Per-tender variance history by employee and outlet
- COGS and gross margin (item-level now, precise once recipes land)
- Period-over-period comparison

### J) Cross-cutting Guarantees

Architectural requirements, built in from the first migration:

|Concern|Approach|
|---|---|
|**Offline-first POS**|Client-generated UUIDs, local sync queue, device-bound long-lived refresh token, PIN-unlocked local session|
|**Idempotency**|`Idempotency-Key` header required on order creation, payment, and refund; keys stored in Redis with the persisted response|
|**Immutable line snapshots**|Product name, unit price, and modifier prices copied onto the line at sale time — later price changes never rewrite past revenue|
|**Money as integers**|`BIGINT` in the smallest currency unit. No floats anywhere, including in transit|
|**Business day ≠ calendar day**|Configurable per-outlet cutoff; every order carries `business_date`; all timestamps stored UTC, rendered in outlet timezone|
|**Audit log**|Every void, discount, price override, refund, OTP verification, and device enrollment: who, when, why, and the value moved|
|**Event bus**|Transactional outbox + worker. `order.paid` fans out to notification, inventory deduction, and report rollup independently|

---

## 📚 Further spec

Read only the file a task needs:

- `context/spec/data-model.md` — full PostgreSQL DDL; read before writing a migration or a repository query for a new table
- `context/spec/flows-and-api.md` — architecture, auth flow, order lifecycle and offline POS diagrams, indicative API surface; read before adding endpoints
- `context/spec/stack-and-roadmap.md` — tech stack, target project layout, MVP / phase roadmap

---

## ⚠️ Known Design Risks

| Risk | Mitigation |
| --- | --- |
| **OTP cost and abuse** | Every request costs real money on the WhatsApp API. Cooldown, per-identifier and per-IP quotas, attempt limits — build these with the first endpoint, not later |
| **OTP unusable offline** | Device enrolled once via OTP; daily access is a local PIN unlocking a device-bound session. Login never requires connectivity |
| **Redis loss logs everyone out** | AOF persistence; treat Redis as stateful infrastructure, not a disposable cache |
| **Refresh token theft** | Rotation with family-level reuse detection; `auth_events` gives a durable trail |
| **No cash means no ground truth** | Every in-store tender is now an unverified claim. Per-tender settlement reconciliation and void/discount reporting are the only controls — make both mandatory |
| **Refunds cannot be given on the spot** | Explicit refund workflow with request → approve → transfer → reference capture; customers must be told the timeline |
| Order numbers repeat across shifts | Support lookup is by number **+ business date + outlet**; reports never group by order number |
| Midtrans webhook arrives before order is persisted | Webhook handler tolerates unknown order IDs and retries with backoff |
| Offline POS drifts from server catalog | Catalog bundle is versioned per device; stale-version syncs rejected |
| Report rollups drift from source data | Nightly reconciliation job recomputes summaries from `order_lines` and alerts on mismatch |

---

🍽️ **One backend. Three clients. One version of the truth.**
