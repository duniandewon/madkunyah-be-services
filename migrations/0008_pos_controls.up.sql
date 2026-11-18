-- POS Controls (pos-controls-spec §8): line discounts, order discounts and
-- price overrides on open bills. order_lines.discount_amount,
-- order_lines.discount_reason and orders.discount_total (0007) now carry
-- data; their meaning is unchanged. audit_logs gains line.discount,
-- line.discount_removed, price.override, price.override_removed,
-- order.discount and order.discount_removed (free text).

ALTER TABLE order_lines
  ADD COLUMN discount_type              TEXT CHECK (discount_type IN ('percent', 'fixed')),
  ADD COLUMN discount_value             BIGINT CHECK (discount_value > 0),
  ADD COLUMN discounted_by              UUID REFERENCES staff(id),
  ADD COLUMN discount_approved_by       UUID REFERENCES staff(id),
  ADD COLUMN discounted_at              TIMESTAMPTZ,
  -- This line's part of the order discount (FR-TOT-13).
  ADD COLUMN order_discount_share       BIGINT NOT NULL DEFAULT 0 CHECK (order_discount_share >= 0),
  -- The Quote price, kept while an override replaces unit_price.
  ADD COLUMN original_unit_price        BIGINT CHECK (original_unit_price >= 0),
  ADD COLUMN price_override_reason      TEXT,
  ADD COLUMN price_overridden_by        UUID REFERENCES staff(id),
  ADD COLUMN price_override_approved_by UUID REFERENCES staff(id),
  ADD COLUMN price_overridden_at        TIMESTAMPTZ,

  -- The discount fields are all set or all null; discount_reason (0007)
  -- joins the group.
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

  -- The override fields are all set or all null.
  ADD CONSTRAINT order_lines_override_all_or_none CHECK (
    (original_unit_price IS NULL) = (price_override_reason IS NULL) AND
    (original_unit_price IS NULL) = (price_overridden_by IS NULL) AND
    (original_unit_price IS NULL) = (price_override_approved_by IS NULL) AND
    (original_unit_price IS NULL) = (price_overridden_at IS NULL));

-- A fixed order discount clamped by a smaller subtotal keeps its
-- discount_value, so discount_total <= discount_value is not a constraint.
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
