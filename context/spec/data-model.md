# MadKunyah — Data Model (PostgreSQL DDL)

Split out of `context/project-overview.md`. The migrations in `migrations/` are the source of truth for tables already built.

## 🗄️ Data Model (PostgreSQL DDL)

> Starting point — **will evolve**. Indexes and some lookup tables omitted for readability. Note there is no `password_hash` column anywhere in this schema.

### Organisation & Access

```sql
CREATE TABLE outlets (
  id                  UUID PRIMARY KEY,
  name                TEXT NOT NULL,
  timezone            TEXT NOT NULL DEFAULT 'Asia/Jakarta',
  business_day_cutoff TIME NOT NULL DEFAULT '04:00',
  order_prefix        TEXT NOT NULL DEFAULT 'POS',
  address             TEXT,
  phone               TEXT,
  is_active           BOOLEAN NOT NULL DEFAULT TRUE,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE outlet_hours (
  id          UUID PRIMARY KEY,
  outlet_id   UUID NOT NULL REFERENCES outlets(id),
  day_of_week SMALLINT NOT NULL,          -- 0=Sunday
  opens_at    TIME NOT NULL,
  closes_at   TIME NOT NULL,
  UNIQUE (outlet_id, day_of_week)
);

-- One POS device per outlet today. Kept as its own table because
-- refresh tokens and offline sync are bound to a physical device.
CREATE TABLE devices (
  id              UUID PRIMARY KEY,
  outlet_id       UUID NOT NULL REFERENCES outlets(id),
  name            TEXT NOT NULL,
  device_fingerprint TEXT UNIQUE,
  enrolled_by     UUID REFERENCES staff(id),
  enrolled_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at    TIMESTAMPTZ,
  catalog_version INT NOT NULL DEFAULT 0,  -- rejects stale-version syncs
  is_active       BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE staff (
  id         UUID PRIMARY KEY,
  email      TEXT UNIQUE,
  phone      TEXT UNIQUE,                 -- OTP delivery target
  full_name  TEXT NOT NULL,
  pin_hash   TEXT,                        -- LOCAL device unlock only, not a login credential
  is_active  BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (email IS NOT NULL OR phone IS NOT NULL)
);

CREATE TABLE roles (
  id          UUID PRIMARY KEY,
  code        TEXT UNIQUE NOT NULL,       -- cashier | supervisor | manager | owner
  name        TEXT NOT NULL,
  permissions JSONB NOT NULL DEFAULT '[]'
);

-- One person can hold different roles at different outlets.
CREATE TABLE staff_outlet_roles (
  staff_id  UUID NOT NULL REFERENCES staff(id),
  outlet_id UUID NOT NULL REFERENCES outlets(id),
  role_id   UUID NOT NULL REFERENCES roles(id),
  PRIMARY KEY (staff_id, outlet_id, role_id)
);

CREATE TABLE customers (
  id          UUID PRIMARY KEY,
  phone       TEXT UNIQUE NOT NULL,
  email       TEXT UNIQUE,
  full_name   TEXT,
  verified_at TIMESTAMPTZ,                -- set on first successful OTP verify
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Security audit trail. OTP codes and refresh tokens live in Redis;
-- this is the durable record of what happened.
CREATE TABLE auth_events (
  id           UUID PRIMARY KEY,
  subject_type TEXT NOT NULL,             -- staff | customer
  subject_id   UUID,                      -- null if identifier was unknown
  identifier   TEXT NOT NULL,
  event        TEXT NOT NULL,             -- otp.requested | otp.verified | otp.failed
                                          -- | token.refreshed | token.reuse_detected
                                          -- | device.enrolled | session.revoked
  device_id    UUID REFERENCES devices(id),
  ip_address   INET,
  user_agent   TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### Catalog

```sql
CREATE TABLE categories (
  id         UUID PRIMARY KEY,
  name       TEXT NOT NULL,
  sort_order INT NOT NULL DEFAULT 0,
  is_active  BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE products (
  id          UUID PRIMARY KEY,
  category_id UUID REFERENCES categories(id),
  sku         TEXT UNIQUE,
  name        TEXT NOT NULL,
  description TEXT,
  image_url   TEXT,
  base_price  BIGINT NOT NULL,            -- smallest currency unit
  cost_price  BIGINT,                     -- owner-visible only
  is_active   BOOLEAN NOT NULL DEFAULT TRUE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE product_variants (
  id          UUID PRIMARY KEY,
  product_id  UUID NOT NULL REFERENCES products(id),
  name        TEXT NOT NULL,              -- 'Large', 'Iced'
  price_delta BIGINT NOT NULL DEFAULT 0,
  sku         TEXT UNIQUE,
  sort_order  INT NOT NULL DEFAULT 0,
  is_active   BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE modifier_groups (
  id          UUID PRIMARY KEY,
  name        TEXT NOT NULL,              -- 'Sugar level', 'Add-ons'
  is_required BOOLEAN NOT NULL DEFAULT FALSE,
  min_select  INT NOT NULL DEFAULT 0,
  max_select  INT NOT NULL DEFAULT 1
);

CREATE TABLE modifiers (
  id                UUID PRIMARY KEY,
  modifier_group_id UUID NOT NULL REFERENCES modifier_groups(id),
  name              TEXT NOT NULL,
  price_delta       BIGINT NOT NULL DEFAULT 0,
  sort_order        INT NOT NULL DEFAULT 0,
  is_active         BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE product_modifier_groups (
  product_id        UUID NOT NULL REFERENCES products(id),
  modifier_group_id UUID NOT NULL REFERENCES modifier_groups(id),
  sort_order        INT NOT NULL DEFAULT 0,
  PRIMARY KEY (product_id, modifier_group_id)
);

-- Per-outlet, per-channel override. Absence = inherit product defaults.
CREATE TABLE product_channel_settings (
  id             UUID PRIMARY KEY,
  product_id     UUID NOT NULL REFERENCES products(id),
  outlet_id      UUID NOT NULL REFERENCES outlets(id),
  channel        TEXT NOT NULL,           -- web | pos | all
  price          BIGINT,                  -- null = use base_price
  is_available   BOOLEAN NOT NULL DEFAULT TRUE,
  is_sold_out    BOOLEAN NOT NULL DEFAULT FALSE,
  available_from TIME,
  available_to   TIME,
  UNIQUE (product_id, outlet_id, channel)
);
```

### Inventory

```sql
-- Deliberately separate from products. Today a stock item IS a finished
-- product; tomorrow it can be an ingredient. Nothing downstream changes.
CREATE TABLE stock_items (
  id            UUID PRIMARY KEY,
  name          TEXT NOT NULL,
  unit          TEXT NOT NULL,            -- pcs | g | ml
  cost_per_unit BIGINT,
  is_active     BOOLEAN NOT NULL DEFAULT TRUE
);

-- The recipe-ready hinge. v1: one row per product, quantity 1.
CREATE TABLE product_stock_links (
  id                 UUID PRIMARY KEY,
  product_id         UUID NOT NULL REFERENCES products(id),
  product_variant_id UUID REFERENCES product_variants(id),
  modifier_id        UUID REFERENCES modifiers(id),
  stock_item_id      UUID NOT NULL REFERENCES stock_items(id),
  quantity           NUMERIC(12,3) NOT NULL DEFAULT 1
);

CREATE TABLE outlet_stock (
  outlet_id      UUID NOT NULL REFERENCES outlets(id),
  stock_item_id  UUID NOT NULL REFERENCES stock_items(id),
  quantity       NUMERIC(12,3) NOT NULL DEFAULT 0,
  low_threshold  NUMERIC(12,3),
  block_on_empty BOOLEAN NOT NULL DEFAULT FALSE,
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (outlet_id, stock_item_id)
);

-- Append-only ledger. Never updated, never deleted.
CREATE TABLE stock_movements (
  id            UUID PRIMARY KEY,
  outlet_id     UUID NOT NULL REFERENCES outlets(id),
  stock_item_id UUID NOT NULL REFERENCES stock_items(id),
  quantity      NUMERIC(12,3) NOT NULL,   -- signed
  reason        TEXT NOT NULL,            -- sale | restock | waste | transfer | adjustment | refund
  order_id      UUID,
  staff_id      UUID REFERENCES staff(id),
  note          TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### Shifts & Order Numbering

```sql
-- No opening float, no drawer: this business takes no cash.
CREATE TABLE shifts (
  id            UUID PRIMARY KEY,
  outlet_id     UUID NOT NULL REFERENCES outlets(id),
  device_id     UUID REFERENCES devices(id),
  business_date DATE NOT NULL,
  opened_by     UUID NOT NULL REFERENCES staff(id),
  opened_at     TIMESTAMPTZ NOT NULL,
  closed_by     UUID REFERENCES staff(id),
  closed_at     TIMESTAMPTZ,
  status        TEXT NOT NULL DEFAULT 'open'   -- open | closed
);

-- Only one shift may be open per outlet at a time.
CREATE UNIQUE INDEX one_open_shift_per_outlet
  ON shifts (outlet_id) WHERE status = 'open';

-- Atomic sequence allocation: UPDATE ... RETURNING last_value + 1.
CREATE TABLE order_sequences (
  shift_id   UUID NOT NULL REFERENCES shifts(id),
  prefix     TEXT NOT NULL,               -- 'WEB' | 'POS'
  last_value INT NOT NULL DEFAULT 0,
  PRIMARY KEY (shift_id, prefix)
);

-- Settlement check at close. Every in-store tender is an unverified
-- claim, so this is the only control that exists on them.
CREATE TABLE shift_tender_reconciliations (
  id              UUID PRIMARY KEY,
  shift_id        UUID NOT NULL REFERENCES shifts(id),
  tender          TEXT NOT NULL,          -- qris_online | qris_static | card_edc
  expected_amount BIGINT NOT NULL,
  actual_amount   BIGINT NOT NULL,
  variance        BIGINT GENERATED ALWAYS AS (actual_amount - expected_amount) STORED,
  note            TEXT,
  recorded_by     UUID NOT NULL REFERENCES staff(id),
  recorded_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (shift_id, tender)
);
```

### Orders

```sql
CREATE TABLE orders (
  id                  UUID PRIMARY KEY,   -- client-generated, offline-safe
  outlet_id           UUID NOT NULL REFERENCES outlets(id),
  shift_id            UUID REFERENCES shifts(id),
  device_id           UUID REFERENCES devices(id),

  -- Display label only. NEVER a foreign key, NEVER a lookup key.
  order_prefix        TEXT NOT NULL,
  order_sequence      INT  NOT NULL,
  order_number        TEXT NOT NULL,      -- 'POS-001'

  channel             TEXT NOT NULL,      -- web | pos
  fulfillment_summary TEXT NOT NULL,      -- derived: dine_in | takeaway | mixed
  status              TEXT NOT NULL,      -- see state machine below

  customer_id         UUID REFERENCES customers(id),   -- null for guests
  guest_name          TEXT,
  guest_phone         TEXT,
  tracking_token      TEXT UNIQUE,        -- opaque; the public tracking key

  table_number        TEXT,
  pickup_at           TIMESTAMPTZ,        -- null = ASAP
  note                TEXT,

  subtotal            BIGINT NOT NULL DEFAULT 0,
  discount_total      BIGINT NOT NULL DEFAULT 0,
  service_charge      BIGINT NOT NULL DEFAULT 0,  -- computed on dine-in lines
  packaging_fee       BIGINT NOT NULL DEFAULT 0,  -- computed on takeaway lines
  tax_total           BIGINT NOT NULL DEFAULT 0,
  grand_total         BIGINT NOT NULL DEFAULT 0,
  tax_breakdown       JSONB NOT NULL DEFAULT '{}', -- proof of how the total was reached

  business_date       DATE NOT NULL,
  placed_at           TIMESTAMPTZ,
  created_by          UUID REFERENCES staff(id),
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- The number is unique only within its shift.
  UNIQUE (outlet_id, shift_id, order_prefix, order_sequence)
);

CREATE TABLE order_lines (
  id                 UUID PRIMARY KEY,
  order_id           UUID NOT NULL REFERENCES orders(id) ON DELETE CASCADE,

  product_id         UUID NOT NULL REFERENCES products(id),
  product_variant_id UUID REFERENCES product_variants(id),

  -- Immutable snapshot. Reporting reads THESE, never the products table.
  product_name       TEXT   NOT NULL,
  variant_name       TEXT,
  unit_price         BIGINT NOT NULL,
  unit_cost          BIGINT,

  quantity           INT    NOT NULL CHECK (quantity > 0),

  -- Per-line, not per-order. The core rule of this system.
  fulfillment_type   TEXT   NOT NULL,     -- dine_in | takeaway

  discount_amount    BIGINT NOT NULL DEFAULT 0,
  discount_reason    TEXT,
  line_total         BIGINT NOT NULL,
  note               TEXT,
  status             TEXT NOT NULL DEFAULT 'active',  -- active | voided
  voided_by          UUID REFERENCES staff(id),
  void_reason        TEXT,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE order_line_modifiers (
  id            UUID PRIMARY KEY,
  order_line_id UUID NOT NULL REFERENCES order_lines(id) ON DELETE CASCADE,
  modifier_id   UUID NOT NULL REFERENCES modifiers(id),
  modifier_name TEXT   NOT NULL,          -- snapshot
  price_delta   BIGINT NOT NULL           -- snapshot
);

CREATE TABLE order_status_events (
  id          UUID PRIMARY KEY,
  order_id    UUID NOT NULL REFERENCES orders(id),
  from_status TEXT,
  to_status   TEXT NOT NULL,
  reason      TEXT,
  staff_id    UUID REFERENCES staff(id),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### Payments & Refunds

```sql
CREATE TABLE payments (
  id            UUID PRIMARY KEY,
  order_id      UUID NOT NULL REFERENCES orders(id),
  method        TEXT NOT NULL,            -- qris_online | qris_static | card_edc
  amount        BIGINT NOT NULL,
  status        TEXT NOT NULL,            -- pending | settled | expired | failed

  provider      TEXT,                     -- 'midtrans' for online
  provider_ref  TEXT,                     -- transaction_id
  provider_fee  BIGINT,                   -- gross sales ≠ payout
  qr_string     TEXT,
  qr_expires_at TIMESTAMPTZ,
  external_ref  TEXT,                     -- RRN from EDC slip or static QRIS app

  recorded_by   UUID REFERENCES staff(id),
  settled_at    TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- With cash gone, on-the-spot refunds are impossible. Manual bank
-- transfer is the main path, not the edge case.
CREATE TABLE refunds (
  id            UUID PRIMARY KEY,
  order_id      UUID NOT NULL REFERENCES orders(id),
  payment_id    UUID REFERENCES payments(id),
  amount        BIGINT NOT NULL,
  refund_method TEXT NOT NULL,            -- bank_transfer | provider
  bank_name     TEXT,
  account_name  TEXT,
  account_no    TEXT,
  reference     TEXT,                     -- transfer reference, entered by staff
  status        TEXT NOT NULL,            -- requested | approved | paid | rejected
  reason        TEXT NOT NULL,
  requested_by  UUID NOT NULL REFERENCES staff(id),
  approved_by   UUID REFERENCES staff(id),
  paid_at       TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE payment_webhook_events (
  id           UUID PRIMARY KEY,
  provider     TEXT NOT NULL,
  event_id     TEXT NOT NULL,
  payload      JSONB NOT NULL,
  signature_ok BOOLEAN NOT NULL,
  processed_at TIMESTAMPTZ,
  received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (provider, event_id)             -- idempotency against retries
);
```

### Operational Infrastructure

```sql
CREATE TABLE audit_logs (
  id          UUID PRIMARY KEY,
  outlet_id   UUID REFERENCES outlets(id),
  actor_id    UUID,
  actor_type  TEXT NOT NULL,              -- staff | customer | system
  action      TEXT NOT NULL,              -- order.void | line.discount | price.override | refund.issue
  entity_type TEXT NOT NULL,
  entity_id   UUID NOT NULL,
  value_delta BIGINT,                     -- money moved, for fraud reporting
  reason      TEXT,
  before      JSONB,
  after       JSONB,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE outbox_events (
  id             UUID PRIMARY KEY,
  aggregate_type TEXT NOT NULL,
  aggregate_id   UUID NOT NULL,
  event_type     TEXT NOT NULL,           -- order.paid | order.ready | stock.low
  payload        JSONB NOT NULL,
  published_at   TIMESTAMPTZ,
  attempts       INT NOT NULL DEFAULT 0,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

> `idempotency_keys` and refresh/OTP state live in Redis, not Postgres — see the key schema in section B.

### Reporting Read Models

Populated by outbox workers. The dashboard never touches transactional tables.

```sql
CREATE TABLE daily_sales_summary (
  outlet_id      UUID NOT NULL REFERENCES outlets(id),
  business_date  DATE NOT NULL,
  channel        TEXT NOT NULL,
  order_count    INT    NOT NULL DEFAULT 0,
  gross_sales    BIGINT NOT NULL DEFAULT 0,
  discount_total BIGINT NOT NULL DEFAULT 0,
  net_sales      BIGINT NOT NULL DEFAULT 0,
  tax_total      BIGINT NOT NULL DEFAULT 0,
  cogs_total     BIGINT NOT NULL DEFAULT 0,
  void_count     INT    NOT NULL DEFAULT 0,
  void_amount    BIGINT NOT NULL DEFAULT 0,
  refund_amount  BIGINT NOT NULL DEFAULT 0,
  dine_in_sales  BIGINT NOT NULL DEFAULT 0,
  takeaway_sales BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (outlet_id, business_date, channel)
);

CREATE TABLE product_sales_daily (
  outlet_id     UUID NOT NULL REFERENCES outlets(id),
  business_date DATE NOT NULL,
  product_id    UUID NOT NULL REFERENCES products(id),
  quantity_sold INT    NOT NULL DEFAULT 0,
  gross_sales   BIGINT NOT NULL DEFAULT 0,
  cogs_total    BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (outlet_id, business_date, product_id)
);

CREATE TABLE hourly_sales_summary (
  outlet_id     UUID NOT NULL REFERENCES outlets(id),
  business_date DATE NOT NULL,
  hour_of_day   SMALLINT NOT NULL,
  order_count   INT    NOT NULL DEFAULT 0,
  gross_sales   BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (outlet_id, business_date, hour_of_day)
);
```
