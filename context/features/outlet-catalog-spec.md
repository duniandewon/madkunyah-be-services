# Outlet Catalog — Feature Spec

## 1. Introduction

This feature makes the global catalog work per outlet and per channel. It adds:

- per-outlet, per-channel overrides of price, visibility, and the daily availability window
- **sold-out** state, toggled by any staff member at the outlet, from the dashboard or the POS
- the public outlet list and web menu for the online shop
- the versioned POS catalog bundle, so the POS can sell for days offline
- caching for the menu and the bundle

A sold-out toggle at the counter reaches the online shop on its next request.

**Depends on:**

- **Catalog Core**: catalog tables, vocabulary (§1.5 there), and the `Quote` interface, which this feature extends
- outlet-scoped authorization (`internal/authz`), including global roles
- POS devices, the `pos` audience, and `RequireActingStaff` (auth slice 3). The POS sold-out route is the first to mount `RequireActingStaff`

**Related:** **Product Images** adds image URLs to the menu and the bundle.

### 1.1 Goals

- Each outlet can price and show products differently on web and POS, without duplicating the catalog.
- A sold-out toggle reaches the online shop on its next request.
- The POS can sell for days offline from a local bundle and still knows windows and sold-out expiry.
- The online shop and the POS never see `cost_price`.
- Outlet price changes and sold-out toggles leave an attributable trail.

### 1.2 In scope

- Per-outlet, per-channel overrides: price, visibility, daily window
- Sold-out state per outlet for products, variants, and modifiers
- Extending `Quote` with overrides and sold-out
- Public outlet list and web menu
- POS catalog bundle with content-hash versioning
- Redis caching for the menu and the bundle
- Auditing, metrics, and error contract for the above

### 1.3 Out of scope

- Per-outlet or per-channel pricing of variants and modifiers. Only the product's own price is overridable
- Removing a product-level window at a single outlet (FR-OUT-6)
- Push updates to the online shop (SSE or WebSocket). Clients re-fetch; checkout re-validates
- Offline sold-out toggles through `/v1/sync/batch`. They move there when sync is built (FR-SO-9)
- Outlet management (hours, address editing). This feature only reads outlets
- Stock-based blocking (`block_on_empty`) → inventory feature

---

## 2. Actors and Access

| Actor | Client | What they do | Audience | Permission |
| --- | --- | --- | --- | --- |
| Anyone | Online shop | Browse outlets and a menu | none (public) | — |
| Owner | Dashboard | Set outlet prices, plus everything a manager does | `staff` | `catalog.manage` (global) |
| Manager | Dashboard | Set visibility and windows at their outlets | `staff` | `product.availability.manage` (outlet) |
| Any outlet staff | Dashboard or POS | Mark items sold out or back in stock | `staff` or `pos` | `product.sold_out.toggle` (outlet) |
| Any staff with a role at the outlet | Dashboard | View the outlet's effective catalog | `staff` | any role at the outlet |
| POS device | POS | Download the outlet bundle | `pos` | device session |

No new permission strings are needed.

---

## 3. Functional Requirements

Requirement IDs are stable and referenced by the acceptance criteria in §6.

### 3.1 Outlet and channel settings

- **FR-OUT-1** Overrides live in `product_channel_settings`, keyed by `(product_id, outlet_id, channel)`, where `channel ∈ web | pos`. Every override column is nullable, and **null means inherit** the product default. A missing row means inherit everything. There is no `all` channel value: a change that applies to both channels writes both rows.
- **FR-OUT-2** **Resolution** for product *p* at outlet *o* on channel *c*:

  | Attribute | Rule |
  | --- | --- |
  | Visible | category active **and** *p* active **and** `coalesce(row.is_visible, p.is_<c>_visible)` |
  | Price | `coalesce(row.price, p.base_price)` |
  | Window | the row's window if set, else the product's window if set, else none (always in window) |

- **FR-OUT-3** **Opt-out by default.** New outlets and new products inherit. A new product appears at every outlet, on every channel its product-level flags allow, until an outlet overrides it.
- **FR-OUT-4** **Permissions per field:**
  - `is_visible` and the window require `product.availability.manage` at that outlet.
  - `price` requires `catalog.manage`. Outlet prices are **owner-only**; managers cannot change them.
  - If the request touches a field the caller may not change, the **whole request** is rejected with `403 forbidden`. There are no partial writes.
- **FR-OUT-5** `PUT /v1/outlets/{outlet_id}/products/{product_id}/channels/{channel}` sets the row. `DELETE` on the same path removes it, so every field returns to inheriting.
- **FR-OUT-6** An outlet window **replaces** the product window; it cannot remove it. Removing the product-level window removes it everywhere. Window rules are those of Catalog Core FR-AVL-1 to FR-AVL-5.
- **FR-OUT-7** Only the product price can be overridden. Variant and modifier deltas are global.
- **FR-OUT-8** Settings can only be written for an active outlet and an existing product. An archived product's settings can be edited, so they are ready when it is restored.
- **FR-OUT-9** `GET /v1/outlets/{outlet_id}/catalog` returns every product with, per channel, its override row (if any) and its resolved visible, price, and window. `cost_price` follows Catalog Core FR-PRD-7.

### 3.2 Sold out

- **FR-SO-1** Sold out is **per outlet and applies to both channels**: running out of croissants is a physical fact, not a channel setting. It can target a product, a variant, or a modifier. It lives in `outlet_sold_out`, not in the channel settings.
- **FR-SO-2** **Set state** with `{ item: {product_id | variant_id | modifier_id}, sold_out, scope }`, where `scope ∈ today | indefinite` (default `today`):
  - `today` sets `sold_out_until` to the outlet's **next business-day cutoff** (`outlets.business_day_cutoff`, in the outlet's timezone)
  - `indefinite` sets `sold_out_until = null`
  - `sold_out: false` clears it
- **FR-SO-3** An item is sold out exactly when its row has `is_sold_out` and (`sold_out_until` is null or later than the evaluation time). Expiry needs no background job.
- **FR-SO-4** **Effect on orderable:**
  - a sold-out product is not orderable, but stays in the menu with a flag
  - a sold-out variant cannot be chosen; the product is not orderable if all its active variants are sold out
  - a sold-out modifier cannot be chosen; the product is not orderable if any required group has fewer than `min_select` modifiers left
- **FR-SO-5** **Routes and permission.** Both require `product.sold_out.toggle` at the outlet.
  - Dashboard: `PUT /v1/outlets/{outlet_id}/sold-out`, `staff` audience.
  - POS: `PUT /v1/pos/sold-out`, `pos` audience, with `X-Acting-Staff-Id`. The outlet comes from the device token. The acting staff member's permission is checked by **server-side authz**, not the roster.
- **FR-SO-6** **Idempotency and ordering.**
  - Setting the current state again returns `200` with `applied: false` and writes no audit row.
  - POS requests carry `changed_at`, the device time of the tap. A `changed_at` in the future is clamped to server time.
  - If `changed_at` is older than the stored one, the request is ignored (`200`, `applied: false`, current state returned). This stops a late offline toggle from overwriting a newer one.
  - Dashboard requests use server time.
- **FR-SO-7** The target must exist and be active, or the request returns `404 not_found`.
- **FR-SO-8** A committed toggle is reflected on the **next** menu or bundle request (FR-CACHE-2).
- **FR-SO-9** **Offline POS.** A toggle applies on the device immediately. Until `/v1/sync/batch` exists, the POS sends it to `PUT /v1/pos/sold-out` on reconnect. When sync is built, it moves into the sync payload with the same semantics.
- **FR-SO-10** `GET /v1/outlets/{outlet_id}/sold-out` lists the items currently sold out at the outlet, with `sold_out_until`, who changed it, and when.

### 3.3 Quote extension

- **FR-OQ-1** `Quote` applies FR-OUT-2 resolution (visibility, price, window) for the request's outlet and channel. Its signature and callers do not change.
- **FR-OQ-2** `Quote` checks sold-out (FR-SO-3) at `At`, for the product, the chosen variant, and each chosen modifier.
- **FR-OQ-3** New problem kinds: `product_sold_out`, `variant_sold_out`, `modifier_sold_out`. `Strict` and `Record` modes behave as in Catalog Core FR-QTE-6.
- **FR-OQ-4** `Quote` still reads Postgres only, never the cache.

### 3.4 Public outlets and web menu

- **FR-MENU-1** `GET /v1/outlets` is public. It returns active outlets with `id`, `name`, `address`, `phone`, and `timezone`.
- **FR-MENU-2** `GET /v1/outlets/{id}/menu` is public. An unknown or inactive outlet returns `404 not_found`.
- **FR-MENU-3** The menu contains only items that are **visible on `web`** at that outlet. Archived and invisible items are absent entirely, not flagged. Categories with no visible products are omitted. Ordering follows Catalog Core (FR-CAT-5, product `sort_order`, then name).
- **FR-MENU-4** Each product carries its effective web price and derived state, evaluated at request time:

  ```json
  {
    "id": "…", "name": "Es Kopi Susu", "description": "…",
    "price": 22000,
    "orderable": false,
    "sold_out": false,
    "in_window": false,
    "window": { "from": "07:00", "to": "11:00" },
    "variants": [ { "id": "…", "name": "Large", "price_delta": 5000, "sold_out": false } ],
    "modifier_groups": [
      { "id": "…", "name": "Sugar level", "required": true, "min_select": 1, "max_select": 1,
        "modifiers": [ { "id": "…", "name": "Less sugar", "price_delta": 0, "sold_out": false } ] }
    ]
  }
  ```

- **FR-MENU-5** The menu never contains `cost_price`, SKUs, staff IDs, or override metadata.
- **FR-MENU-6** The response carries an `ETag` over the evaluated body and `Cache-Control: no-cache`. Browsers revalidate on every view and receive `304` when nothing changed.
- **FR-MENU-7** The menu is informational. Checkout always re-validates through `Quote`, so a stale browser view cannot buy a sold-out item.

### 3.5 POS catalog bundle

- **FR-BND-1** `GET /v1/pos/catalog` requires a `pos` token for an active device. The outlet is the device's outlet.
- **FR-BND-2** The bundle contains:
  - categories, and products **visible on `pos`** at the outlet, with their effective POS price
  - variants, attached modifier groups (with rules), and modifiers
  - **raw** windows and sold-out rows with `sold_out_until`, not evaluated flags
  - the outlet's `timezone` and `business_day_cutoff`, so the POS can evaluate locally

  It never contains `cost_price` or web-only items.
- **FR-BND-3** Because it holds raw rules instead of evaluated state, the bundle does not change as time passes. Its `version` is the SHA-256 of its canonical JSON, the same approach as the roster version.
- **FR-BND-4** The response carries `ETag: "<version>"`. A request with a matching `If-None-Match` returns `304`.
- **FR-BND-5** On both `200` and `304`, the server records `devices.catalog_version` and `devices.catalog_fetched_at`.
- **FR-BND-6** **Client contract** (documented for the POS team): fetch on reconnect, on every sync, and at least every 5 minutes while online. Evaluate windows and sold-out locally against the device clock.
- **FR-BND-7** **The bundle version is informational, never a rejection reason.** No server component rejects an operation because it was made under an older bundle. A sale made offline from an old bundle has already happened, so sync accepts it and prices it with `Quote` in `Record` mode, which reports any drift. This replaces the overview's "stale-version syncs rejected".

### 3.6 Caching

- **FR-CACHE-1** The menu (before time evaluation) and the POS bundle are cached in Redis, keyed by a global generation counter:

  ```
  catalog:gen                        → counter, no TTL
  catalog:menu:{gen}:{outlet_id}     → menu document, before time evaluation   TTL CATALOG_CACHE_TTL (5m)
  catalog:bundle:{gen}:{outlet_id}   → bundle JSON + version                   TTL CATALOG_CACHE_TTL (5m)
  ```

- **FR-CACHE-2** Every catalog write, including Catalog Core writes, runs `INCR catalog:gen` **after** its transaction commits. Readers read `catalog:gen` **before** querying Postgres. With that ordering, a stale document can only ever be stored under an old generation, which no one reads again.
- **FR-CACHE-3** Time-dependent flags (`in_window`, `sold_out`, `orderable`) are evaluated per request from the cached document, so expiry needs no invalidation.
- **FR-CACHE-4** The counter is global, so a write at one outlet invalidates every outlet's cache. A rebuilt bundle with unchanged content keeps the same hash, so the POS still gets `304`.

### 3.7 Authorization summary

| Route | Audience | Requirement | No role at outlet |
| --- | --- | --- | --- |
| `GET /v1/outlets`, `GET /v1/outlets/{id}/menu` | public | — | — |
| `GET /v1/outlets/{outlet_id}/catalog`, `GET …/sold-out` | `staff` | any role at the outlet | `404` |
| `PUT`/`DELETE …/outlets/{outlet_id}/products/{product_id}/channels/{channel}` | `staff` | FR-OUT-4 | `404` |
| `PUT /v1/outlets/{outlet_id}/sold-out` | `staff` | `product.sold_out.toggle` | `404` |
| `GET /v1/pos/catalog` | `pos` | active device | — |
| `PUT /v1/pos/sold-out` | `pos` + acting staff | `product.sold_out.toggle` | `403` |

FR-AZ-5 applies to every outlet-scoped `staff` route. Global role holders are treated as having a role at every outlet.

### 3.8 Audit

- **FR-OAUD-1** Every write in this feature that changes something writes one `audit_logs` row in the **same transaction**, with `outlet_id` set:
  - `actor_type = staff`, `actor_id` = the caller, or the acting staff member on POS
  - `before`/`after` holding only the changed fields
  - `value_delta` = new − old effective price when an outlet `price` changes
- **FR-OAUD-2** Actions:

  | Entity | Actions |
  | --- | --- |
  | `product` (outlet-scoped) | `outlet_product.updated`, `outlet_product.reset` |
  | `product`, `product_variant`, or `modifier` | `sold_out.set`, `sold_out.cleared` |

- **FR-OAUD-3** POS sold-out rows include `device_id` and the client `changed_at` in `after`.
- **FR-OAUD-4** No-op writes (FR-SO-6) write no audit row.

---

## 4. Non-Functional Requirements

### 4.1 Security

- **NFR-SEC-1** `cost_price` never appears in the menu, the bundle, or any POS route. A test asserts it.
- **NFR-SEC-2** Public routes are rate-limited per IP (`CATALOG_PUBLIC_RATE_LIMIT`, default 120 per minute) → `429 rate_limited`.
- **NFR-SEC-3** Public responses expose no archived IDs, staff IDs, SKUs, or override metadata.
- **NFR-SEC-4** JSON request bodies are capped at 64 KB.

### 4.2 Performance

Targets assume 500 products and 1,000 modifiers.

- **NFR-PERF-1** Menu: p95 ≤ 50 ms on a cache hit, ≤ 250 ms on a miss. `304`: p95 ≤ 20 ms.
- **NFR-PERF-2** POS bundle ≤ 300 KB gzipped. `304`: p95 ≤ 20 ms.
- **NFR-PERF-3** `Quote` keeps the Catalog Core target (p95 ≤ 10 ms for 20 lines) with overrides and sold-out applied.
- **NFR-PERF-4** A committed sold-out toggle is visible on the menu and bundle within 1 second.

### 4.3 Availability and resilience

- **NFR-AV-1** Unlike auth, the catalog does **not** fail closed on Redis:

  | Failure | Behaviour |
  | --- | --- |
  | Redis down on read | Bypass the cache and read Postgres. Emit a metric. |
  | `INCR catalog:gen` fails after commit | Log and emit a metric. Staleness is bounded by `CATALOG_CACHE_TTL`. The write still succeeds. |
  | Postgres down | `503 catalog_unavailable` |

- **NFR-AV-2** POS selling never depends on the server. The POS sells from its last bundle.

### 4.4 Operability and testability

- **NFR-OPS-1** All limits are configuration: `CATALOG_CACHE_TTL`, `CATALOG_PUBLIC_RATE_LIMIT`.
- **NFR-OPS-2** The clock is injectable, so window, cutoff, and sold-out expiry tests are deterministic, including across midnight and the business-day cutoff.
- **NFR-OPS-3** The resolution matrix (FR-OUT-2) has a table-driven unit test. Integration tests use `testcontainers-go` with real Postgres and Redis.
- **NFR-OPS-4** The OpenAPI spec documents every route and error code in §5.
- **NFR-OPS-5** The feature emits the Prometheus metrics in §7.

---

## 5. Edge Cases and Error Handling

### 5.1 Error contract

Uses the existing envelope and Catalog Core's `validation_failed`. Codes used here:

| HTTP | `code` | When | Notes |
| --- | --- | --- | --- |
| 304 | — | `If-None-Match` matches (menu, bundle) | No body |
| 400 | `validation_failed` | Bad window, bad price, bad scope, more than one item target | |
| 401 | `device_revoked` | POS route from a revoked device | Existing device check |
| 403 | `forbidden` | Has a role at the outlet, lacks the permission (incl. FR-OUT-4) | |
| 404 | `not_found` | Unknown or archived target, inactive outlet, or no role at the outlet | FR-AZ-5 |
| 429 | `rate_limited` | Public route limiter | |
| 503 | `catalog_unavailable` | Postgres down | |

### 5.2 Edge cases

**Outlet settings**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-1 | Product hidden on web globally, but an outlet row sets `is_visible = true` for web | Visible on web at that outlet only |
| E-2 | Outlet row sets only `price`; product is globally POS-only | Still POS-only. `is_visible` is null, so it inherits |
| E-3 | Manager sends `{ price, is_visible }` | `403 forbidden`, nothing written |
| E-4 | Manager at Outlet A edits Outlet B settings | `404 not_found` |
| E-5 | Owner sets a web price at one outlet | `Quote` on `web` there uses it; `pos` there and both channels elsewhere use `base_price` |
| E-6 | New outlet created | Every active, product-level-visible product appears on its menu with no extra setup |
| E-7 | Web pre-order at 10:55 for pickup at 11:30, breakfast item ending 11:00 | Depends on `At` chosen by the orders feature. With `At = pickup_at`, rejected as `outside_window` |

**Sold out**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-8 | Cashier marks a product sold out (`today`) at 14:00; cutoff is 04:00 | Sold out until 04:00 the next day, then automatically back, with no job |
| E-9 | Marked sold out at 02:00, before the 04:00 cutoff | Until 04:00 the **same** calendar day, the next cutoff |
| E-10 | Oat milk (modifier) sold out; the "Milk" group is required with `min_select = 1` and oat is its only option | Product shows `orderable: false` |
| E-11 | All variants of a product sold out | Product shows `orderable: false` |
| E-12 | POS toggles offline at 10:00; HQ clears it at 10:05; POS reconnects at 10:10 | POS request ignored (`applied: false`) because its `changed_at` is older |
| E-13 | POS clock is an hour fast | `changed_at` clamped to server time on receipt |
| E-14 | Same toggle sent twice (retry) | Second returns `applied: false`, no second audit row |
| E-15 | Customer has a sold-out item in their cart from a stale menu | Checkout `Quote` rejects the line with `product_sold_out` |
| E-16 | `PUT /v1/pos/sold-out` from a revoked device | `401 device_revoked` |
| E-17 | Request targets both a product and a variant | `400 validation_failed` |
| E-18 | Pre-order for tomorrow on an item sold out `today` | Valid: `Quote` evaluates sold-out at `At`, which is after `sold_out_until` |

**Bundle and caching**

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-19 | Unrelated outlet toggles sold out; POS at another outlet polls | Generation bumps, bundle rebuilds, hash unchanged → `304` |
| E-20 | POS offline for two days | Sells from its last bundle; evaluates windows and sold-out expiry locally. On reconnect, fetches a new bundle |
| E-21 | Offline sale at a price the owner has since changed | Accepted at sync; `Quote` `Record` mode reports the drift (FR-BND-7) |
| E-22 | Redis down | Menu and bundle served from Postgres; slower, still correct |
| E-23 | Write commits, then `INCR catalog:gen` fails | Stale reads for up to `CATALOG_CACHE_TTL`; metric fires. Checkout is still correct because `Quote` reads Postgres |

---

## 6. Acceptance Criteria

Each criterion is an automated integration test unless marked *(manual)*.

### Outlet settings

- **AC-1** *(FR-OUT-2)* A table-driven test covers the resolution matrix: no row, row with each field null or set, product flags true or false, for both channels.
- **AC-2** *(FR-OUT-4)* A manager can set `is_visible` and the window at their outlet. A manager setting `price` gets `403` and nothing is written. An owner can set all three.
- **AC-3** *(FR-OUT-4, FR-AZ-5)* A manager at A writing settings at B gets `404`.
- **AC-4** *(FR-OUT-5)* After `DELETE` on a settings row, the effective values equal the product defaults.
- **AC-5** *(FR-OUT-3)* A product created after an outlet exists appears on that outlet's menu with no settings row.

### Sold out

- **AC-6** *(FR-SO-2, FR-SO-3)* A `today` toggle expires at the next business-day cutoff in outlet time, for toggles before and after the cutoff on the same calendar day. An `indefinite` toggle does not expire.
- **AC-7** *(FR-SO-1)* A product marked sold out from the POS shows `sold_out: true` on the web menu for the same outlet, and not at other outlets.
- **AC-8** *(FR-SO-4)* A sold-out variant set and a sold-out required-modifier set each make the product `orderable: false`. A sold-out optional modifier does not.
- **AC-9** *(FR-SO-5)* `PUT /v1/pos/sold-out` without `X-Acting-Staff-Id`, or with a staff member who has no role at the device's outlet, is rejected. With a valid cashier it succeeds, and the audit row names the cashier and the device.
- **AC-10** *(FR-SO-6)* A POS toggle with an older `changed_at` than the stored one returns `applied: false` and changes nothing. A repeated identical toggle writes no audit row.
- **AC-11** *(FR-SO-8, NFR-PERF-4)* Immediately after a toggle commits, the next menu request reflects it.

### Quote

- **AC-12** *(FR-OQ-1)* For a product with an outlet web price override, a variant, and two modifiers, `unit_price` equals override + variant delta + both modifier deltas.
- **AC-13** *(FR-OQ-2, FR-OQ-3)* Each of the three sold-out problem kinds is produced from a matching input, and none is produced when `At` is after `sold_out_until`.
- **AC-14** *(FR-OQ-4)* With a stale cache entry deliberately planted, `Quote` returns the Postgres price.

### Menu and bundle

- **AC-15** *(FR-MENU-3)* The web menu contains exactly the web-visible items at the outlet: no POS-only items, no archived items, no items from archived categories.
- **AC-16** *(NFR-SEC-1, FR-MENU-5, FR-BND-2)* `cost_price` is absent from the menu, the bundle, and every POS route.
- **AC-17** *(FR-MENU-6, FR-BND-4)* Repeating a request with the returned `ETag` gives `304`. Changing a visible item's price gives `200` with a new `ETag`.
- **AC-18** *(FR-BND-3)* Two bundle builds at different clock times, with no catalog writes between them, have identical versions.
- **AC-19** *(FR-BND-5)* After a `200` or a `304`, the device's `catalog_version` and `catalog_fetched_at` are updated.
- **AC-20** *(FR-MENU-2)* An inactive or unknown outlet returns `404` for its menu.

### Audit and resilience

- **AC-21** *(FR-OAUD-1)* Changing an outlet web price from inherited 20,000 to 23,000 writes one audit row with `outlet_id` set and `value_delta = 3000`.
- **AC-22** *(NFR-AV-1)* With Redis stopped, the menu and bundle still return correct content.
- **AC-23** *(manual)* On target POS hardware, the bundle for a 500-product catalog loads and renders the menu in under 1 second.

---

## 7. Metrics

Exposed as Prometheus metrics, labelled by `outlet_id`, `channel`, or `route` where meaningful.

### 7.1 Health and experience

| Metric | Definition | Target / alert |
| --- | --- | --- |
| Menu latency | p95 of `GET …/menu`, by cache hit or miss | ≤ 50 ms hit, ≤ 250 ms miss |
| Cache hit ratio | Hits ÷ reads, for menu and bundle | ≥ 90%; alert below 70% over 30 min |
| Bundle `304` ratio | `304` ÷ bundle requests | Track; a sudden drop means the version is unstable (FR-BND-3) |
| POS catalog staleness | Active devices whose `catalog_fetched_at` is older than 15 min | Alert per device after 30 min |
| Cache generation bump failures | Failed `INCR catalog:gen` | Any occurrence alerts |
| Checkout sold-out rejections | `Quote` `product_sold_out` in `Strict` mode on `web` | A rise means menus are going stale in clients |

### 7.2 Control signals

| Metric | Definition | Alert |
| --- | --- | --- |
| Outlet price changes | Audit rows `outlet_product.updated` with `value_delta` | Listed on the owner dashboard |
| Sold-out toggles | Per outlet, per staff member, per day | Outliers surfaced; very frequent toggles may hide stock problems |
| Indefinite sold-out items | Items sold out with no expiry for more than 7 days | Weekly report; likely should be archived or hidden |

---

## 8. Data Model — Migration `outlet_catalog`

Differences from the overview DDL are marked `-- Δ`.

```sql
-- Per-outlet, per-channel override. Every override column: NULL = inherit.
CREATE TABLE product_channel_settings (
  product_id     UUID NOT NULL REFERENCES products(id),
  outlet_id      UUID NOT NULL REFERENCES outlets(id),
  channel        TEXT NOT NULL CHECK (channel IN ('web', 'pos')),   -- Δ 'all' removed
  price          BIGINT CHECK (price >= 0),        -- NULL = products.base_price
  is_visible     BOOLEAN,                          -- Δ renamed from is_available; NULL = product default
  available_from TIME,                             -- both NULL = product window
  available_to   TIME,
  updated_by     UUID NOT NULL REFERENCES staff(id),   -- Δ
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),   -- Δ
  PRIMARY KEY (product_id, outlet_id, channel),        -- Δ natural key replaces surrogate id
  CHECK ((available_from IS NULL) = (available_to IS NULL)),
  CHECK (available_from IS NULL OR available_from <> available_to)
  -- Δ is_sold_out moved to outlet_sold_out (sold out is per outlet, not per channel)
);

-- Δ New. Sold-out state per outlet, for exactly one of product / variant / modifier.
-- Rows persist when cleared, so changed_at can order late offline toggles (FR-SO-6).
CREATE TABLE outlet_sold_out (
  id                 UUID PRIMARY KEY,
  outlet_id          UUID NOT NULL REFERENCES outlets(id),
  product_id         UUID REFERENCES products(id),
  product_variant_id UUID REFERENCES product_variants(id),
  modifier_id        UUID REFERENCES modifiers(id),
  is_sold_out        BOOLEAN NOT NULL,
  sold_out_until     TIMESTAMPTZ,                  -- NULL = until cleared
  changed_at         TIMESTAMPTZ NOT NULL,         -- device time (clamped) for POS, server time otherwise
  changed_by         UUID NOT NULL REFERENCES staff(id),
  changed_via        TEXT NOT NULL CHECK (changed_via IN ('pos', 'dashboard')),
  device_id          UUID REFERENCES devices(id),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (num_nonnulls(product_id, product_variant_id, modifier_id) = 1)
);
CREATE UNIQUE INDEX outlet_sold_out_product  ON outlet_sold_out (outlet_id, product_id)         WHERE product_id IS NOT NULL;
CREATE UNIQUE INDEX outlet_sold_out_variant  ON outlet_sold_out (outlet_id, product_variant_id) WHERE product_variant_id IS NOT NULL;
CREATE UNIQUE INDEX outlet_sold_out_modifier ON outlet_sold_out (outlet_id, modifier_id)        WHERE modifier_id IS NOT NULL;

-- Δ Devices: the bundle version is a content hash, not a counter (FR-BND-3).
-- Adjust to match how the existing migrations created this column, if at all.
ALTER TABLE devices ALTER COLUMN catalog_version DROP DEFAULT;
ALTER TABLE devices ALTER COLUMN catalog_version DROP NOT NULL;
ALTER TABLE devices ALTER COLUMN catalog_version TYPE TEXT USING NULL;
ALTER TABLE devices ADD COLUMN catalog_fetched_at TIMESTAMPTZ;
```

**Redis:** additions to the key schema (FR-CACHE-1).

```
catalog:gen                        → counter                          no TTL
catalog:menu:{gen}:{outlet_id}     → menu document (unevaluated)      TTL 5m
catalog:bundle:{gen}:{outlet_id}   → bundle JSON + version            TTL 5m
```

**API:**

```
# Public
GET    /v1/outlets
GET    /v1/outlets/{id}/menu

# Dashboard — per outlet
GET    /v1/outlets/{outlet_id}/catalog
PUT    /v1/outlets/{outlet_id}/products/{product_id}/channels/{channel}
DELETE /v1/outlets/{outlet_id}/products/{product_id}/channels/{channel}
GET    /v1/outlets/{outlet_id}/sold-out
PUT    /v1/outlets/{outlet_id}/sold-out

# POS
GET    /v1/pos/catalog
PUT    /v1/pos/sold-out
```

---

## 9. Risks and Accepted Trade-offs

| Risk | Status |
| --- | --- |
| **Menu staleness in the browser.** A customer can view a menu, then the item sells out. | Accepted. `Quote` re-validates at checkout from Postgres. There is no push. |
| **POS sells from an old bundle offline.** Prices or availability may have changed at HQ. | Accepted by design (FR-BND-7). Order lines carry the POS's snapshot, and `Record` mode reports the drift. |
| **Outlet windows can replace but not remove a product window** (FR-OUT-6). | Accepted. Rare; remove the product-level window and set windows per outlet instead. |
| **Global cache generation.** Any write invalidates every outlet's cache. | Accepted at this scale. Bundle hashes keep the POS on `304` when its content didn't change. |
| **Device clocks decide the order of offline sold-out toggles.** | Bounded by clamping future times to server time (FR-SO-6). A slow clock can only lose to a newer HQ toggle, which is the safe outcome. |
| **Sold-out reaches the POS by polling.** An HQ toggle can take up to the poll interval to appear on the POS. | Accepted. Most toggles start at the POS itself, where they apply instantly. |
