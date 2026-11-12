-- Shifts and per-shift order numbering (shifts-spec §8).
-- No opening float, no drawer: this business takes no cash.

CREATE TABLE shifts (
  id               UUID PRIMARY KEY,                       -- client-generated, the retry key (FR-OPN-2)
  outlet_id        UUID NOT NULL REFERENCES outlets(id),
  device_id        UUID NOT NULL REFERENCES devices(id),   -- always opened on a POS
  business_date    DATE NOT NULL,                          -- fixed at open (FR-SH-6)
  opened_by        UUID NOT NULL REFERENCES staff(id),
  opened_at        TIMESTAMPTZ NOT NULL,
  closed_by        UUID REFERENCES staff(id),
  closed_at        TIMESTAMPTZ,
  closed_device_id UUID REFERENCES devices(id),            -- always closed on a POS (FR-SH-8)
  status           TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- The close fields are all set or all null.
  CHECK ((status = 'open') = (closed_at IS NULL)),
  CHECK ((status = 'open') = (closed_by IS NULL)),
  CHECK ((status = 'open') = (closed_device_id IS NULL))
);

-- Only one shift may be open per outlet at a time (FR-SH-3).
CREATE UNIQUE INDEX one_open_shift_per_outlet
  ON shifts (outlet_id) WHERE status = 'open';

-- Dashboard list by business date (FR-RD-2).
CREATE INDEX shifts_outlet_business_date_idx
  ON shifts (outlet_id, business_date DESC, opened_at DESC);

-- Atomic sequence allocation: UPDATE ... RETURNING last_value (FR-SEQ-2).
-- Rows are created at shift open, one per prefix (FR-OPN-3).
CREATE TABLE order_sequences (
  shift_id   UUID NOT NULL REFERENCES shifts(id),
  prefix     TEXT NOT NULL,               -- outlets.order_prefix | 'WEB'
  last_value INT  NOT NULL DEFAULT 0 CHECK (last_value >= 0),
  PRIMARY KEY (shift_id, prefix)
);
