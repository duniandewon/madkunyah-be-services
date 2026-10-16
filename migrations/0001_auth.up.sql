-- Organisation & access tables needed by authentication.
-- Includes the auth-spec §8 changes: nullable customer phone, global role
-- grants, device revocation columns and staff.deactivated_at.

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

CREATE TABLE staff (
  id             UUID PRIMARY KEY,
  email          TEXT UNIQUE,
  phone          TEXT UNIQUE,              -- E.164, OTP delivery target
  full_name      TEXT NOT NULL,
  pin_hash       TEXT,                     -- LOCAL device unlock only, not a login credential
  is_active      BOOLEAN NOT NULL DEFAULT TRUE,
  deactivated_at TIMESTAMPTZ,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (email IS NOT NULL OR phone IS NOT NULL)
);

-- One POS device per outlet today. Kept as its own table because refresh
-- tokens and offline sync are bound to a physical device.
CREATE TABLE devices (
  id                 UUID PRIMARY KEY,
  outlet_id          UUID NOT NULL REFERENCES outlets(id),
  name               TEXT NOT NULL,
  device_fingerprint TEXT UNIQUE,
  enrolled_by        UUID REFERENCES staff(id),
  enrolled_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at       TIMESTAMPTZ,
  catalog_version    INT NOT NULL DEFAULT 0,
  is_active          BOOLEAN NOT NULL DEFAULT TRUE,
  revoked_at         TIMESTAMPTZ,
  revoked_by         UUID REFERENCES staff(id)
);

CREATE TABLE roles (
  id          UUID PRIMARY KEY,
  code        TEXT UNIQUE NOT NULL,        -- cashier | supervisor | manager | owner
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

-- Global (non-outlet) role grants, e.g. owner.
CREATE TABLE staff_global_roles (
  staff_id UUID NOT NULL REFERENCES staff(id),
  role_id  UUID NOT NULL REFERENCES roles(id),
  PRIMARY KEY (staff_id, role_id)
);

-- Customers sign up with a phone OR an email.
CREATE TABLE customers (
  id          UUID PRIMARY KEY,
  phone       TEXT UNIQUE,
  email       TEXT UNIQUE,
  full_name   TEXT,
  verified_at TIMESTAMPTZ,                 -- set on first successful OTP verify
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT customers_identifier_present CHECK (phone IS NOT NULL OR email IS NOT NULL)
);

-- Security audit trail. OTP codes and refresh tokens live in Redis; this is
-- the durable record of what happened. Append-only.
CREATE TABLE auth_events (
  id           UUID PRIMARY KEY,
  subject_type TEXT NOT NULL,              -- staff | customer | device
  subject_id   UUID,                       -- null if identifier was unknown
  identifier   TEXT NOT NULL,
  event        TEXT NOT NULL,              -- otp.requested | otp.verified | otp.failed
                                           -- | token.refreshed | token.reuse_detected
                                           -- | device.enrolled | session.revoked
                                           -- | pin.set | pin.reset | pin.failed
  device_id    UUID REFERENCES devices(id),
  ip_address   INET,
  user_agent   TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX auth_events_subject_idx ON auth_events (subject_type, subject_id, created_at);
CREATE INDEX auth_events_identifier_idx ON auth_events (identifier, created_at);

-- The four fixed v1 roles (auth-spec FR-AZ-1). Permissions are data so custom
-- roles can come later without a schema change.
INSERT INTO roles (id, code, name, permissions) VALUES
  (gen_random_uuid(), 'cashier', 'Cashier', '[
    "order.create", "payment.record", "order.void.unpaid", "product.sold_out.toggle"
  ]'),
  (gen_random_uuid(), 'supervisor', 'Supervisor', '[
    "order.create", "payment.record", "order.void.unpaid", "product.sold_out.toggle",
    "order.void.paid", "order.discount", "order.price_override",
    "shift.open", "shift.close", "shift.report.view", "refund.request"
  ]'),
  (gen_random_uuid(), 'manager', 'Outlet Manager', '[
    "order.create", "payment.record", "order.void.unpaid", "product.sold_out.toggle",
    "order.void.paid", "order.discount", "order.price_override",
    "shift.open", "shift.close", "shift.report.view", "refund.request",
    "refund.approve", "product.availability.manage", "stock.manage",
    "staff.manage", "device.enroll", "device.revoke", "report.outlet.view"
  ]'),
  (gen_random_uuid(), 'owner', 'Owner', '[
    "order.create", "payment.record", "order.void.unpaid", "product.sold_out.toggle",
    "order.void.paid", "order.discount", "order.price_override",
    "shift.open", "shift.close", "shift.report.view", "refund.request",
    "refund.approve", "product.availability.manage", "stock.manage",
    "staff.manage", "device.enroll", "device.revoke", "report.outlet.view",
    "report.all.view", "cost.view", "margin.view",
    "catalog.manage", "outlet.manage", "audit.view", "auth_events.view"
  ]');
