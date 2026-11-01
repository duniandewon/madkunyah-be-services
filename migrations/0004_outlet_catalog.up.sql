-- Outlet Catalog (outlet-catalog spec §8): per-outlet, per-channel overrides
-- of the global catalog, outlet sold-out state, and the POS bundle version
-- as a content hash. audit_logs gains outlet_product.updated/reset and
-- sold_out.set/cleared (free text, no constraint to change).

-- Per-outlet, per-channel override. Every override column: NULL = inherit.
-- A missing row inherits everything (opt-out by default).
CREATE TABLE product_channel_settings (
  product_id     UUID NOT NULL REFERENCES products(id),
  outlet_id      UUID NOT NULL REFERENCES outlets(id),
  channel        TEXT NOT NULL CHECK (channel IN ('web', 'pos')),
  price          BIGINT CHECK (price >= 0),        -- NULL = products.base_price
  is_visible     BOOLEAN,                          -- NULL = product default
  available_from TIME,                             -- both NULL = product window
  available_to   TIME,
  updated_by     UUID NOT NULL REFERENCES staff(id),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (product_id, outlet_id, channel),
  CHECK ((available_from IS NULL) = (available_to IS NULL)),
  CHECK (available_from IS NULL OR available_from <> available_to)
);
CREATE INDEX product_channel_settings_outlet_idx ON product_channel_settings (outlet_id, channel);

-- Sold-out state per outlet (both channels), for exactly one of product,
-- variant or modifier. Rows persist when cleared, so changed_at can order
-- late offline toggles.
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

-- The bundle version is a content hash, not a counter. It is informational:
-- nothing is ever rejected for an older version.
ALTER TABLE devices ALTER COLUMN catalog_version DROP DEFAULT;
ALTER TABLE devices ALTER COLUMN catalog_version DROP NOT NULL;
ALTER TABLE devices ALTER COLUMN catalog_version TYPE TEXT USING NULL;
ALTER TABLE devices ADD COLUMN catalog_fetched_at TIMESTAMPTZ;
