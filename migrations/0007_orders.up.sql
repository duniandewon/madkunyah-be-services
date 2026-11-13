-- Orders Core (orders-spec §8): POS orders while online, open bills,
-- payments, and the outbox. Web columns (customer_id, guest_name,
-- guest_phone, tracking_token, pickup_at), refunds and
-- payment_webhook_events come with later specs. audit_logs gains
-- order.line_voided and order.cancelled (free text, no constraint to change).

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
