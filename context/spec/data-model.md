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

Built by migration `0007_orders` (orders-spec §8). Web columns (`customer_id`, `guest_name`, `guest_phone`, `tracking_token` UNIQUE, `pickup_at`) are added by Web Orders; `shift_id` may become nullable there for queued web orders. `service_charge` and `packaging_fee` stay 0 until charges are introduced.

```sql
CREATE TABLE orders (
  id                  UUID PRIMARY KEY,                    -- client-generated, retry key (FR-PLC-6)
  outlet_id           UUID NOT NULL REFERENCES outlets(id),
  shift_id            UUID NOT NULL REFERENCES shifts(id),
  device_id           UUID REFERENCES devices(id),         -- null for web orders

  -- Display label only. NEVER a foreign key, NEVER a lookup key on its own.
  order_prefix        TEXT NOT NULL,
  order_sequence      INT  NOT NULL CHECK (order_sequence > 0),
  order_number        TEXT NOT NULL,                       -- 'POS-001'

  channel             TEXT NOT NULL CHECK (channel IN ('web', 'pos')),
  fulfillment_summary TEXT NOT NULL CHECK (fulfillment_summary IN ('dine_in', 'takeaway', 'mixed')),
  status              TEXT NOT NULL CHECK (status IN (
                        'draft', 'pending_payment', 'placed', 'preparing', 'ready',
                        'completed', 'cancelled', 'expired', 'refunded')),

  table_number        TEXT,
  note                TEXT,

  subtotal            BIGINT NOT NULL DEFAULT 0 CHECK (subtotal >= 0),
  discount_total      BIGINT NOT NULL DEFAULT 0 CHECK (discount_total >= 0),
  service_charge      BIGINT NOT NULL DEFAULT 0 CHECK (service_charge >= 0),
  packaging_fee       BIGINT NOT NULL DEFAULT 0 CHECK (packaging_fee >= 0),
  tax_total           BIGINT NOT NULL DEFAULT 0 CHECK (tax_total >= 0),
  grand_total         BIGINT NOT NULL DEFAULT 0 CHECK (grand_total >= 0),
  tax_breakdown       JSONB  NOT NULL DEFAULT '{}',

  business_date       DATE NOT NULL,                       -- the shift's
  placed_at           TIMESTAMPTZ,
  paid_at             TIMESTAMPTZ,                         -- paid is not a status (FR-ORD-3)
  created_by          UUID REFERENCES staff(id),           -- null for web orders
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- The number is unique only within its shift.
  UNIQUE (outlet_id, shift_id, order_prefix, order_sequence),
  -- A counter order always has a device and a staff member.
  CHECK (channel <> 'pos' OR (device_id IS NOT NULL AND created_by IS NOT NULL))
);

CREATE INDEX orders_shift_status_idx ON orders (shift_id, status, created_at DESC);  -- guard, POS list
CREATE INDEX orders_outlet_business_date_idx ON orders (outlet_id, business_date DESC, created_at DESC);

CREATE TABLE order_lines (
  id                 UUID PRIMARY KEY,                     -- client-generated, retry key (FR-ADD-3)
  order_id           UUID NOT NULL REFERENCES orders(id),  -- no cascade: orders are never deleted
  -- Position on the order, in the order the POS sent the lines. Lines
  -- added together share created_at, so this keeps the ticket's order.
  line_no            INT  NOT NULL CHECK (line_no > 0),

  product_id         UUID NOT NULL REFERENCES products(id),
  product_variant_id UUID REFERENCES product_variants(id),

  -- Immutable snapshot. Reporting reads THESE, never the products table.
  product_name       TEXT   NOT NULL,
  variant_name       TEXT,
  category_id        UUID   NOT NULL REFERENCES categories(id),
  category_name      TEXT   NOT NULL,
  unit_price         BIGINT NOT NULL CHECK (unit_price >= 0),
  unit_cost          BIGINT,

  quantity           INT    NOT NULL CHECK (quantity > 0),
  fulfillment_type   TEXT   NOT NULL CHECK (fulfillment_type IN ('dine_in', 'takeaway')),

  discount_amount    BIGINT NOT NULL DEFAULT 0 CHECK (discount_amount >= 0),  -- unused until POS Controls
  discount_reason    TEXT,
  line_total         BIGINT NOT NULL CHECK (line_total >= 0),
  note               TEXT,

  status             TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'voided')),
  voided_by          UUID REFERENCES staff(id),
  voided_at          TIMESTAMPTZ,
  void_reason        TEXT,
  created_by         UUID REFERENCES staff(id),            -- who added the line (FR-ADD-4)
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (order_id, line_no),
  -- The void fields are all set or all null.
  CHECK ((status = 'voided') = (voided_at IS NOT NULL)),
  CHECK ((status = 'voided') = (void_reason IS NOT NULL))
);

CREATE TABLE order_line_modifiers (
  id            UUID PRIMARY KEY,
  order_line_id UUID NOT NULL REFERENCES order_lines(id),  -- no cascade
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
```

### Payments & Refunds

```sql
CREATE TABLE payments (
  id            UUID PRIMARY KEY,                          -- client-generated on POS, retry key (FR-PAY-5)
  order_id      UUID NOT NULL REFERENCES orders(id),
  method        TEXT NOT NULL CHECK (method IN ('qris_online', 'qris_static', 'card_edc')),
  amount        BIGINT NOT NULL CHECK (amount >= 0),
  status        TEXT NOT NULL CHECK (status IN ('pending', 'settled', 'expired', 'failed')),

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
-- One settled payment per order (FR-PAY-6). No split payments.
CREATE UNIQUE INDEX one_settled_payment_per_order ON payments (order_id) WHERE status = 'settled';

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
  action      TEXT NOT NULL,              -- order.line_voided | order.cancelled | line.discount | price.override | refund.issue
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
  event_type     TEXT NOT NULL,                            -- order.placed | order.updated | order.cancelled | order.paid
  payload        JSONB NOT NULL,
  published_at   TIMESTAMPTZ,
  attempts       INT NOT NULL DEFAULT 0,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The worker's queue: unpublished rows, oldest first.
CREATE INDEX outbox_events_unpublished_idx ON outbox_events (created_at) WHERE published_at IS NULL;
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
