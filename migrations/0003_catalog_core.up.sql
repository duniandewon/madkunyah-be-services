-- Global catalog (catalog-core spec §8). Supersedes the Catalog section of the
-- overview DDL. Outlet overrides and sold-out come with Outlet Catalog,
-- image_key with Product Images, product_stock_links with inventory.
-- Nothing here is ever hard-deleted: order lines and reports reference it.
-- audit_logs.entity_type gains category, product, product_variant,
-- modifier_group and modifier (free text, no constraint to change).

CREATE TABLE categories (
  id         UUID PRIMARY KEY,
  name       TEXT NOT NULL,
  sort_order INT NOT NULL DEFAULT 0,
  is_active  BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX categories_name_active ON categories (lower(name)) WHERE is_active;

CREATE TABLE products (
  id             UUID PRIMARY KEY,
  category_id    UUID NOT NULL REFERENCES categories(id),
  sku            TEXT,
  name           TEXT NOT NULL,
  description    TEXT,
  base_price     BIGINT NOT NULL CHECK (base_price >= 0),   -- tax-exclusive
  cost_price     BIGINT CHECK (cost_price >= 0),            -- owner-visible only
  is_web_visible BOOLEAN NOT NULL DEFAULT TRUE,             -- default for every outlet
  is_pos_visible BOOLEAN NOT NULL DEFAULT TRUE,
  available_from TIME,                                      -- daily window, outlet-local
  available_to   TIME,
  sort_order     INT NOT NULL DEFAULT 0,
  is_active      BOOLEAN NOT NULL DEFAULT TRUE,             -- false = archived
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK ((available_from IS NULL) = (available_to IS NULL)),
  CHECK (available_from IS NULL OR available_from <> available_to)
);
CREATE UNIQUE INDEX products_name_active ON products (category_id, lower(name)) WHERE is_active;
-- Not partial: an archived product's SKU is never reused. Cross-table SKU
-- uniqueness (products vs variants) is enforced in the service.
CREATE UNIQUE INDEX products_sku ON products (lower(sku)) WHERE sku IS NOT NULL;

CREATE TABLE product_variants (
  id          UUID PRIMARY KEY,
  product_id  UUID NOT NULL REFERENCES products(id),
  name        TEXT NOT NULL,
  price_delta BIGINT NOT NULL DEFAULT 0 CHECK (price_delta >= 0),
  sku         TEXT,
  sort_order  INT NOT NULL DEFAULT 0,
  is_active   BOOLEAN NOT NULL DEFAULT TRUE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX product_variants_name_active ON product_variants (product_id, lower(name)) WHERE is_active;
CREATE UNIQUE INDEX product_variants_sku ON product_variants (lower(sku)) WHERE sku IS NOT NULL;
CREATE INDEX product_variants_product_idx ON product_variants (product_id);

CREATE TABLE modifier_groups (
  id         UUID PRIMARY KEY,
  name       TEXT NOT NULL,
  min_select INT NOT NULL DEFAULT 0 CHECK (min_select >= 0),
  max_select INT NOT NULL DEFAULT 1 CHECK (max_select >= 1),
  is_active  BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- required = (min_select >= 1); there is no is_required column.
  CHECK (min_select <= max_select)
);

CREATE TABLE modifiers (
  id                UUID PRIMARY KEY,
  modifier_group_id UUID NOT NULL REFERENCES modifier_groups(id),
  name              TEXT NOT NULL,
  price_delta       BIGINT NOT NULL DEFAULT 0 CHECK (price_delta >= 0),
  sort_order        INT NOT NULL DEFAULT 0,
  is_active         BOOLEAN NOT NULL DEFAULT TRUE,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX modifiers_name_active ON modifiers (modifier_group_id, lower(name)) WHERE is_active;
CREATE INDEX modifiers_group_idx ON modifiers (modifier_group_id);

CREATE TABLE product_modifier_groups (
  product_id        UUID NOT NULL REFERENCES products(id),
  modifier_group_id UUID NOT NULL REFERENCES modifier_groups(id),
  sort_order        INT NOT NULL DEFAULT 0,
  PRIMARY KEY (product_id, modifier_group_id)
);
CREATE INDEX product_modifier_groups_group_idx ON product_modifier_groups (modifier_group_id);
CREATE INDEX products_category_idx ON products (category_id);
