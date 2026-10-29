# Catalog Core — Feature Spec

## 1. Introduction

This feature builds the menu's structure: categories, products, variants, and modifier groups. It gives the owner the dashboard endpoints to maintain them, and it defines how a selected item is validated and priced.

The catalog is **global**. The owner maintains one set of categories, products, variants, and modifier groups for the whole business. Each product carries channel-level defaults (visible on web, visible on POS, a daily availability window) that apply at every outlet.

The catalog is also the **source of order-line snapshots**. Orders don't exist yet, but they will copy product name, price, and modifier prices from the catalog at the moment of sale. This feature defines the internal `Quote` interface that the orders feature will call, so the price rules live in exactly one place.

**Related features:**

- **Outlet Catalog** (next) adds per-outlet price and visibility overrides, sold-out, the public web menu, and the POS catalog bundle. It extends `Quote` without changing its callers.
- **Product Images** adds image upload and storage.

**Depends on:** outlet-scoped authorization (`internal/authz`), including global roles. No dependency on server-side step-up: no catalog action needs a supervisor approval.

### 1.1 Goals

- One catalog, edited once by the owner, shared by every outlet.
- Order lines can be validated, priced, and snapshotted by a single, tested function.
- Catalog edits never rewrite past revenue.
- Every price change leaves an attributable trail, because price manipulation is a fraud vector once cash is gone.
- Cost prices never leave the owner's view.

### 1.2 In scope

- Categories, products, variants, modifier groups, modifiers: create, read, update, archive, restore
- Attaching modifier groups to products
- Product-level channel visibility and daily availability window
- Cost price storage and visibility rules
- Internal `Quote` interface: selection validation, price resolution, line snapshot
- Dashboard read and write endpoints, auditing, metrics, and error contract

### 1.3 Out of scope

- Per-outlet price, visibility, and window overrides; sold-out; public menu; POS bundle; caching → **Outlet Catalog**
- Product images → **Product Images**
- Combos and bundles (*paket*). A combo is modelled as a product with required modifier groups ("choose a drink"); see §9
- Variants combining two axes ("Large Iced") (see FR-VAR-6)
- Modifier quantities ("extra shot × 2"), nested modifiers, variant-specific modifier groups
- Default modifier selections (e.g. "Normal sugar" pre-selected)
- Day-of-week schedules, date-ranged seasonal items, scheduled future price changes
- Multi-language names and descriptions
- Bulk import or export (CSV)
- Stock links, stock-based blocking, and recipe COGS → inventory feature
- POS price override, discounts, tax, and service charge → orders feature

### 1.4 Decisions carried from the project overview

| Topic | Decision |
| --- | --- |
| Tenancy | One global catalog. Outlet-specific facts carry `outlet_id` (Outlet Catalog). |
| Channels | `web` and `pos`. Some items are POS-only. |
| Money | `BIGINT` in the smallest currency unit (rupiah). No floats, including in JSON. |
| Snapshots | Order lines copy name and prices at sale time. Catalog edits never rewrite past revenue. |
| Cost | `cost_price` is owner-only (`cost.view`). |

### 1.5 Vocabulary

"Available" is overloaded in the overview. All catalog features use these five terms:

| Term | Meaning | Feature |
| --- | --- | --- |
| **Archived** | Removed from the catalog. Hidden everywhere, kept for history. `is_active = false`. | Catalog Core |
| **Visible** | Shown on a channel at an outlet. | Product default here; outlet override in Outlet Catalog |
| **In window** | Inside its daily availability window at the evaluation time. | Product default here; outlet override in Outlet Catalog |
| **Sold out** | Temporarily cannot be sold at an outlet, on any channel. | Outlet Catalog |
| **Orderable** | Visible, in window, not sold out, and every required choice can be satisfied. Derived, never stored. | Both |

---

## 2. Actors and Access

| Actor | Client | What they do | Audience | Permission |
| --- | --- | --- | --- | --- |
| Owner | Dashboard | Create and edit the catalog, set prices, see cost | `staff` | `catalog.manage`, `cost.view` (global) |
| Any staff with a role | Dashboard | Read the catalog | `staff` | any role, outlet or global |
| Orders module | internal | Validate and price order lines | — | in-process call |

No new permission strings are needed. Both already exist in the FR-AZ-1 matrix.

---

## 3. Functional Requirements

Requirement IDs are stable and referenced by the acceptance criteria in §6.

### 3.1 Categories

- **FR-CAT-1** Categories are global. Fields: `name` (1–60 characters, trimmed), `sort_order`, `is_active`.
- **FR-CAT-2** Active category names are unique, case-insensitively. A duplicate returns `409 name_taken`.
- **FR-CAT-3** Archiving a category hides it and every product in it from every channel, **without** archiving the products. Restoring it brings them back with their own settings intact. This is the switch for a seasonal menu.
- **FR-CAT-4** Products can be created in, or moved to, an archived category. This lets the owner stage a seasonal menu before launch.
- **FR-CAT-5** Categories are ordered by `sort_order`, then name, everywhere they are listed.

### 3.2 Products

- **FR-PRD-1** Fields:
  - `category_id` (required)
  - `name` (1–100 characters), `description` (≤ 1000 characters)
  - `sku` (optional, ≤ 64 characters)
  - `base_price`, `cost_price` (optional)
  - `is_web_visible`, `is_pos_visible` (both default `true`)
  - `available_from`, `available_to` (optional daily window, §3.5)
  - `sort_order`, `is_active`
- **FR-PRD-2** Active product names are unique within their category, case-insensitively → `409 name_taken`.
- **FR-PRD-3** SKUs are unique case-insensitively across **all products and variants, archived included**, so a SKU is never reused → `409 sku_taken`.
- **FR-PRD-4** **Money:** every price is a JSON integer in rupiah, `0 ≤ value ≤ CATALOG_PRICE_MAX` (default 100,000,000). Strings, floats, and negative numbers return `400 validation_failed`.
- **FR-PRD-5** **Prices are tax-exclusive.** Catalog prices are the price before PB1 and service charge, which the orders feature adds at checkout.
- **FR-PRD-6** **Channel visibility defaults.** `is_web_visible` and `is_pos_visible` apply at every outlet unless an outlet overrides them (Outlet Catalog). Making an item POS-only everywhere means setting `is_web_visible = false`; there is no separate bulk endpoint.
- **FR-PRD-7** **Cost visibility:**
  - Writing `cost_price` requires both `catalog.manage` and `cost.view`.
  - `cost_price` is returned only to callers holding `cost.view`. For everyone else the field is **absent**, not null.
- **FR-PRD-8** **Archive, never delete.** No endpoint hard-deletes a category, product, variant, modifier group, or modifier, because order lines, stock links, and report rows reference them.
  - Archive and restore go through `PATCH { "is_active": false | true }`.
  - Dashboard lists exclude archived items unless `include_archived=true`.
  - Restoring re-checks name uniqueness.
- **FR-PRD-9** Catalog edits never change existing orders. Orders keep their own snapshot (FR-QTE-4).

### 3.3 Variants

- **FR-VAR-1** A variant is one option on a **single axis** that changes the price, such as size.
  - If a product has at least one active variant, every order line for it must choose exactly one.
  - A product with no active variants is sold as-is.
- **FR-VAR-2** Unit price before modifiers = product price + `price_delta`.
- **FR-VAR-3** `price_delta ≥ 0`. The product's own price is the cheapest option. This makes a negative unit price impossible by construction, under every outlet override.
- **FR-VAR-4** Active variant names are unique within their product → `409 name_taken`. SKUs follow FR-PRD-3.
- **FR-VAR-5** Archiving the **last** active variant of a product returns `409 last_active_variant`. Otherwise the product would silently become orderable at its base price with no size chosen. To stop selling variants, archive the product and create a new one.
- **FR-VAR-6** **One variant axis per product.** Use the axis that changes the price most as the variants. Any second choice, such as temperature, is a required modifier group with `min_select = max_select = 1`. Variants never encode combinations like "Large Iced".

### 3.4 Modifier groups and modifiers

- **FR-MOD-1** Modifier groups are global and reusable. They attach to products through `product_modifier_groups` with a per-product `sort_order`. Editing a group affects every product it is attached to.
- **FR-MOD-2** Group rules: `min_select ≥ 0`, `max_select ≥ 1`, `min_select ≤ max_select`. A group is **required** exactly when `min_select ≥ 1`. There is no separate `is_required` column; responses include a derived `required` field.
- **FR-MOD-3** Modifier fields: `name` (1–60 characters), `price_delta ≥ 0`, `sort_order`, `is_active`. Active modifier names are unique within their group → `409 name_taken`.
- **FR-MOD-4** **Selection rules**, enforced by `Quote`:
  - each modifier is selected at most once per line
  - each modifier belongs to a group attached to the product
  - for every attached active group, the number selected is within `[min_select, max_select]`
  - archived modifiers and archived groups cannot be selected
- **FR-MOD-5** **Satisfiability guard.** A write returns `409 modifier_group_unsatisfiable` if, afterwards, any active group attached to an active product would have fewer active modifiers than its `min_select`. This applies to:
  - attaching a group
  - raising `min_select`
  - archiving a modifier

  The error lists the affected products.
- **FR-MOD-6** `PUT /v1/products/{id}/modifier-groups` replaces the product's whole attached set atomically. Archived groups cannot be attached → `400 validation_failed`.
- **FR-MOD-7** Archiving a group that is attached to any active product returns `409 modifier_group_in_use`, listing those products. Detach it first. This stops a required choice from silently disappearing.

### 3.5 Availability windows

- **FR-AVL-1** A window is a daily `[from, to)` interval in the **outlet's local time** (`outlets.timezone`). Both ends are set or neither is, and `from ≠ to`. A product with no window is always in window.
- **FR-AVL-2** `from > to` wraps past midnight: `22:00–02:00` is valid.
- **FR-AVL-3** Windows are evaluated at the **fulfilment time** passed to `Quote` (`At`). The orders feature decides whether `At` is `pickup_at` or now.
- **FR-AVL-4** Windows apply every day.
- **FR-AVL-5** Windows use wall-clock time, independent of outlet opening hours and of the business-day cutoff.

### 3.6 Quote — internal interface for orders

- **FR-QTE-1** `internal/catalog` exposes:

  ```go
  Quote(ctx context.Context, q Querier, req QuoteRequest) (QuoteResult, error)

  type QuoteRequest struct {
      OutletID uuid.UUID
      Channel  Channel     // web | pos
      At       time.Time   // fulfilment time
      Mode     QuoteMode   // Strict | Record
      Lines    []QuoteLine // {ProductID, VariantID *uuid.UUID, ModifierIDs []uuid.UUID}
  }
  ```

  `q` is the caller's transaction. `Quote` **always reads Postgres, never a cache**, so the price that is committed is the price that was checked.
- **FR-QTE-2** An unknown or inactive outlet fails the whole call with `ErrOutletUnavailable`. Everything else is reported per line.
- **FR-QTE-3** Each line is checked for: category and product active; product visible on the channel (`is_<channel>_visible`); in window at `At`; the variant rule (FR-VAR-1); and the modifier rules (FR-MOD-4).
- **FR-QTE-4** Each valid line returns a **snapshot**:
  - `product_name`, `category_id`, `category_name`, `variant_name`
  - `unit_price` = product price + variant delta + Σ modifier deltas, with that breakdown
  - `unit_cost` = `products.cost_price` (may be null; precise cost arrives with recipes)
  - `modifiers[]` = `{id, name, price_delta}`
- **FR-QTE-5** Line problems are reported per line as stable kinds that the orders feature maps to HTTP errors: `product_unavailable`, `outside_window`, `variant_required`, `variant_invalid`, `modifier_invalid`, `modifier_count_invalid`.
- **FR-QTE-6** **Modes:**
  - `Strict` (web checkout, online POS orders): any problem fails the line.
  - `Record` (replaying offline POS sales): the sale already happened physically, so `Quote` returns a snapshot computed from the current catalog **and** the list of problems, without failing. This follows the same principle as FR-STEP-6.
- **FR-QTE-7** `Quote` is the only place the resolution rules live. Outlet Catalog extends it with outlet overrides and sold-out; callers do not change.

### 3.7 Dashboard reads

- **FR-READ-1** `GET /v1/categories` and `GET /v1/modifier-groups` list all items; `include_archived=true` adds archived ones. Modifier groups include their modifiers.
- **FR-READ-2** `GET /v1/products` filters by `category_id`, `q` (case-insensitive substring of the name), and `include_archived`. Results are ordered by category order, then product `sort_order`, then name.
- **FR-READ-3** `GET /v1/products/{id}` returns the product with its variants and its attached modifier groups and their modifiers, in sort order. Archived children are included and marked.
- **FR-READ-4** Unknown IDs return `404 not_found`.

### 3.8 Authorization summary

| Route | Audience | Requirement |
| --- | --- | --- |
| `GET` on categories, products, modifier groups (+ `/{id}`) | `staff` | any role (outlet or global) |
| `POST`/`PATCH`/`PUT` on categories, products, variants, groups, modifiers, attachments | `staff` | `catalog.manage` (global) |
| Writing `cost_price` | `staff` | `catalog.manage` **and** `cost.view` |

A staff member with no role anywhere gets `403 forbidden` on reads. A caller lacking `catalog.manage` gets `403 forbidden` on writes.

### 3.9 Audit

- **FR-CAUD-1** Every catalog write that changes something writes one `audit_logs` row in the **same transaction**:
  - `actor_type = staff`, `actor_id` = the caller, `outlet_id` = null
  - `entity_type`, `entity_id`, and `before`/`after` holding only the changed fields
  - `value_delta` = new − old whenever a price field changed (`base_price`, a variant `price_delta`, or a modifier `price_delta`)
- **FR-CAUD-2** Actions:

  | Entity | Actions |
  | --- | --- |
  | `category` | `category.created`, `category.updated`, `category.archived`, `category.restored` |
  | `product` | `product.created`, `product.updated`, `product.archived`, `product.restored`, `product.groups_changed` |
  | `product_variant` | `variant.created`, `variant.updated`, `variant.archived`, `variant.restored` |
  | `modifier_group` | `modifier_group.created`, `modifier_group.updated`, `modifier_group.archived`, `modifier_group.restored` |
  | `modifier` | `modifier.created`, `modifier.updated`, `modifier.archived`, `modifier.restored` |

- **FR-CAUD-3** A PATCH that changes nothing writes no audit row.

---

## 4. Non-Functional Requirements

### 4.1 Security

- **NFR-SEC-1** `cost_price` appears only in `staff` responses to holders of `cost.view`. A test asserts it is absent for every other role.
- **NFR-SEC-2** JSON request bodies are capped at 64 KB. Every string field has a length limit.

### 4.2 Performance

Targets assume 500 products and 1,000 modifiers.

- **NFR-PERF-1** `Quote` for a 20-line order: p95 ≤ 10 ms against a warm Postgres. It uses a fixed number of queries regardless of line count, never one per line.
- **NFR-PERF-2** `GET /v1/products` (unfiltered) and `GET /v1/products/{id}`: p95 ≤ 100 ms.

### 4.3 Availability

- **NFR-AV-1** This feature uses Postgres only. When Postgres is down, catalog routes return `503 catalog_unavailable`.

### 4.4 Operability and testability

- **NFR-OPS-1** All limits are configuration: `CATALOG_PRICE_MAX`.
- **NFR-OPS-2** The clock is injectable, so window tests are deterministic, including across midnight.
- **NFR-OPS-3** Selection and resolution rules (FR-MOD-4, FR-QTE-3) have table-driven unit tests. Integration tests use `testcontainers-go` with real Postgres.
- **NFR-OPS-4** The OpenAPI spec documents every route and error code in §5.
- **NFR-OPS-5** The feature emits the Prometheus metrics in §7.

---

## 5. Edge Cases and Error Handling

### 5.1 Error contract

The existing error envelope is used unchanged. New codes:

| HTTP | `code` | When | Notes |
| --- | --- | --- | --- |
| 400 | `validation_failed` | Field missing, out of range, wrong type, bad window | `details` maps field → reason. Reuse an existing generic code if the codebase has one |
| 403 | `forbidden` | Lacks the permission | |
| 404 | `not_found` | Unknown target | |
| 409 | `name_taken` | Duplicate active name in its scope | |
| 409 | `sku_taken` | SKU already used by any product or variant | |
| 409 | `last_active_variant` | Archiving a product's last active variant | FR-VAR-5 |
| 409 | `modifier_group_unsatisfiable` | Write would leave a required group with too few modifiers | Lists affected products |
| 409 | `modifier_group_in_use` | Archiving a group still attached to active products | Lists those products |
| 503 | `catalog_unavailable` | Postgres down | |

### 5.2 Edge cases

| # | Case | Expected behaviour |
| --- | --- | --- |
| E-1 | Owner archives a category containing active products | Its products fail `Quote` with `product_unavailable`. Products stay active; restoring the category brings them back |
| E-2 | Product moved into an archived category | Allowed. It is unavailable until the category is restored |
| E-3 | Two active products named "Original" in different categories | Allowed. Uniqueness is per category |
| E-4 | Restoring a product whose name is now taken by another active product | `409 name_taken`; rename first |
| E-5 | New product reuses an archived product's SKU | `409 sku_taken`. SKUs are never reused |
| E-6 | New variant uses a SKU held by a product | `409 sku_taken` |
| E-7 | Archiving the last active variant | `409 last_active_variant` |
| E-8 | Archiving a modifier drops its group below `min_select` | `409 modifier_group_unsatisfiable` |
| E-9 | Archiving a group attached to products | `409 modifier_group_in_use` |
| E-10 | Raising a shared group's `min_select` | Checked against every product it's attached to; rejected if any becomes unsatisfiable |
| E-11 | Price sent as `"22000"` or `22000.5` | `400 validation_failed` |
| E-12 | Window `22:00–02:00`, evaluated at 01:30 | In window |
| E-13 | Window `07:00–11:00`, evaluated at exactly 11:00 | Out of window (`to` is exclusive) |
| E-14 | Window with only `from` set, or `from = to` | `400 validation_failed` |
| E-15 | Two outlets in different timezones, same product window | Each evaluates in its own outlet's local time |
| E-16 | Owner changes a price while a checkout is mid-flight | The order gets the price `Quote` reads inside its own transaction |
| E-17 | `Quote` line selects the same modifier twice | `modifier_invalid` |
| E-18 | `Quote` for a product with variants and no variant chosen | `variant_required` |
| E-19 | Manager PATCHes a product | `403 forbidden` |

---

## 6. Acceptance Criteria

Each criterion is an automated integration test unless marked *(manual)*.

### Structure

- **AC-1** *(FR-PRD-8)* No route hard-deletes a catalog entity. Archived entities disappear from default lists, appear with `include_archived=true`, and can be restored.
- **AC-2** *(FR-CAT-2, FR-PRD-2, FR-VAR-4, FR-MOD-3)* Duplicate active names in each scope return `409 name_taken`. The same name in a different scope succeeds.
- **AC-3** *(FR-PRD-3)* A SKU used by an archived product, or by any variant, is rejected for a new product or variant.
- **AC-4** *(FR-PRD-4, FR-VAR-3, FR-MOD-3)* Negative values, floats, strings, and values above `CATALOG_PRICE_MAX` are rejected for every price field.
- **AC-5** *(FR-VAR-5)* Archiving the last active variant returns `409`. Archiving a non-last variant succeeds.
- **AC-6** *(FR-MOD-5, FR-MOD-7)* Each of the three unsatisfiable writes returns `409 modifier_group_unsatisfiable`. Archiving an attached group returns `409 modifier_group_in_use`.
- **AC-7** *(FR-MOD-6)* Replacing a product's attached groups is atomic: a request containing an archived group changes nothing.
- **AC-8** *(FR-CAT-3)* Archiving a category makes its products fail `Quote` without changing any product's `is_active`.

### Access and cost

- **AC-9** *(FR-PRD-7, NFR-SEC-1)* `cost_price` is present for an owner and absent (not null) for a manager, a supervisor, and a cashier, on list and detail responses.
- **AC-10** *(§3.8)* A manager receives `403` on every write route. A staff member with no role receives `403` on reads.

### Windows and Quote

- **AC-11** *(FR-AVL-1, FR-AVL-2)* With an injected clock: `07:00–11:00` is in window at 07:00 and 10:59 and out at 11:00. `22:00–02:00` is in window at 23:00 and 01:59 and out at 02:00. Times are in the outlet's timezone.
- **AC-12** *(FR-QTE-3, FR-QTE-5)* A table-driven test produces each problem kind exactly once from a matching input.
- **AC-13** *(FR-QTE-4)* For a product with a variant and two modifiers, `unit_price` equals base price + variant delta + both modifier deltas, and the snapshot names match the catalog.
- **AC-14** *(FR-QTE-6)* The same invalid line fails in `Strict` mode and returns a snapshot plus problems in `Record` mode.
- **AC-15** *(FR-QTE-3)* A product with `is_web_visible = false` is `product_unavailable` on `web` and valid on `pos`.
- **AC-16** *(FR-QTE-1)* `Quote` run inside a transaction that has just changed a price (uncommitted) sees the new price.
- **AC-17** *(FR-QTE-2)* An inactive outlet fails the whole call.

### Audit

- **AC-18** *(FR-CAUD-1)* Changing `base_price` from 20,000 to 22,000 writes one audit row with `value_delta = 2000` and only `base_price` in `before`/`after`. A failed write leaves no audit row.
- **AC-19** *(FR-CAUD-3)* A PATCH with unchanged values writes no audit row.

---

## 7. Metrics

Exposed as Prometheus metrics, labelled by `channel`, `mode`, or `route` where meaningful.

| Metric | Definition | Target / alert |
| --- | --- | --- |
| Quote latency | p95 of `Quote`, by line-count bucket | ≤ 10 ms for 20 lines |
| Quote problems | Count by problem kind and mode | Track; feeds the orders feature's dashboards |
| Catalog write rate | Writes per route | Track |
| Price changes | Audit rows with `value_delta`, by entity type and actor | Surfaced in the owner dashboard |
| Price change and revert | A price changed and reverted within 24 h | Report to owner; classic manipulation pattern |

---

## 8. Data Model — Migration `catalog_core`

The catalog tables don't exist yet. This DDL **supersedes the Catalog section of the overview DDL** for these tables. Differences from the overview are marked `-- Δ`. Overrides and sold-out are created by Outlet Catalog; `image_key` by Product Images.

```sql
CREATE TABLE categories (
  id         UUID PRIMARY KEY,
  name       TEXT NOT NULL,
  sort_order INT NOT NULL DEFAULT 0,
  is_active  BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),   -- Δ
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()    -- Δ
);
CREATE UNIQUE INDEX categories_name_active ON categories (lower(name)) WHERE is_active;

CREATE TABLE products (
  id             UUID PRIMARY KEY,
  category_id    UUID NOT NULL REFERENCES categories(id),   -- Δ NOT NULL
  sku            TEXT,
  name           TEXT NOT NULL,
  description    TEXT,
  base_price     BIGINT NOT NULL CHECK (base_price >= 0),   -- tax-exclusive
  cost_price     BIGINT CHECK (cost_price >= 0),            -- owner-visible only
  is_web_visible BOOLEAN NOT NULL DEFAULT TRUE,             -- Δ default for every outlet
  is_pos_visible BOOLEAN NOT NULL DEFAULT TRUE,             -- Δ default for every outlet
  available_from TIME,                     -- Δ default daily window, outlet-local
  available_to   TIME,                     -- Δ
  sort_order     INT NOT NULL DEFAULT 0,   -- Δ
  is_active      BOOLEAN NOT NULL DEFAULT TRUE,             -- false = archived
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK ((available_from IS NULL) = (available_to IS NULL)),
  CHECK (available_from IS NULL OR available_from <> available_to)
  -- Δ image_url removed; Product Images adds image_key
);
CREATE UNIQUE INDEX products_name_active ON products (category_id, lower(name)) WHERE is_active;
CREATE UNIQUE INDEX products_sku ON products (lower(sku)) WHERE sku IS NOT NULL;
-- Cross-table SKU uniqueness (products vs variants) is enforced in the service.

CREATE TABLE product_variants (
  id          UUID PRIMARY KEY,
  product_id  UUID NOT NULL REFERENCES products(id),
  name        TEXT NOT NULL,
  price_delta BIGINT NOT NULL DEFAULT 0 CHECK (price_delta >= 0),   -- Δ non-negative
  sku         TEXT,
  sort_order  INT NOT NULL DEFAULT 0,
  is_active   BOOLEAN NOT NULL DEFAULT TRUE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),   -- Δ
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()    -- Δ
);
CREATE UNIQUE INDEX product_variants_name_active ON product_variants (product_id, lower(name)) WHERE is_active;
CREATE UNIQUE INDEX product_variants_sku ON product_variants (lower(sku)) WHERE sku IS NOT NULL;

CREATE TABLE modifier_groups (
  id         UUID PRIMARY KEY,
  name       TEXT NOT NULL,
  min_select INT NOT NULL DEFAULT 0 CHECK (min_select >= 0),
  max_select INT NOT NULL DEFAULT 1 CHECK (max_select >= 1),
  is_active  BOOLEAN NOT NULL DEFAULT TRUE,          -- Δ
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),     -- Δ
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),     -- Δ
  CHECK (min_select <= max_select)
  -- Δ is_required removed: required = (min_select >= 1)
);

CREATE TABLE modifiers (
  id                UUID PRIMARY KEY,
  modifier_group_id UUID NOT NULL REFERENCES modifier_groups(id),
  name              TEXT NOT NULL,
  price_delta       BIGINT NOT NULL DEFAULT 0 CHECK (price_delta >= 0),   -- Δ non-negative
  sort_order        INT NOT NULL DEFAULT 0,
  is_active         BOOLEAN NOT NULL DEFAULT TRUE,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),   -- Δ
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()    -- Δ
);
CREATE UNIQUE INDEX modifiers_name_active ON modifiers (modifier_group_id, lower(name)) WHERE is_active;

CREATE TABLE product_modifier_groups (
  product_id        UUID NOT NULL REFERENCES products(id),
  modifier_group_id UUID NOT NULL REFERENCES modifier_groups(id),
  sort_order        INT NOT NULL DEFAULT 0,
  PRIMARY KEY (product_id, modifier_group_id)
);
```

- `audit_logs.entity_type` gains `category`, `product`, `product_variant`, `modifier_group`, `modifier`.
- `product_stock_links` is **not** created here. The inventory feature owns it.

**API:** the overview lists `CRUD /v1/products /v1/categories /v1/modifiers`. The concrete surface:

```
GET    /v1/categories                         POST  /v1/categories
GET    /v1/categories/{id}                    PATCH /v1/categories/{id}
GET    /v1/products                           POST  /v1/products
GET    /v1/products/{id}                      PATCH /v1/products/{id}
POST   /v1/products/{id}/variants             PATCH /v1/products/{id}/variants/{vid}
PUT    /v1/products/{id}/modifier-groups
GET    /v1/modifier-groups                    POST  /v1/modifier-groups
GET    /v1/modifier-groups/{id}               PATCH /v1/modifier-groups/{id}
POST   /v1/modifier-groups/{id}/modifiers     PATCH /v1/modifier-groups/{id}/modifiers/{mid}
```

There is no `DELETE` on catalog entities (FR-PRD-8).

---

## 9. Risks and Accepted Trade-offs

| Risk | Status |
| --- | --- |
| **Price manipulation** (lower a price, ring sales, raise it back). | Every price change is audited with `value_delta`, and the change-and-revert pattern is reported (§7). |
| **Last-write-wins on concurrent dashboard edits.** Two owners editing the same product overwrite each other. | Accepted; the audit log shows both writes. Optimistic locking (`If-Match`) can be added later with only a version column. |
| **Category in reports follows the product's current category.** Moving a product regroups its history. | Mitigated by snapshotting `category_id` and `category_name` on the order line (FR-QTE-4). The orders feature should add those columns. |
| **Combos modelled as products with required groups.** Pricing and stock are coarse. | Accepted. Stock deduction for the chosen components works through `product_stock_links.modifier_id` once inventory lands. |
| **`unit_cost` ignores variants and modifiers.** A large drink has the same recorded cost as a regular one. | Accepted. Precise COGS comes from recipes in the inventory work, consistent with the overview. |
| **One variant axis** forces a second choice into a modifier group, which carries no SKU. | Accepted. Stock for the second axis can still be linked through `product_stock_links.modifier_id`. |
