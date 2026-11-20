-- Outlet Management (outlet-crud-spec §7): the database agrees with the
-- field rules the API enforces. Existing rows use POS / Asia/Jakarta /
-- 04:00 and satisfy them. The timezone has no CHECK: the write path checks
-- it with time.LoadLocation and AT TIME ZONE instead. audit_logs gains outlet.created,
-- outlet.updated, outlet.deactivated and outlet.activated (free text).

ALTER TABLE outlets
  ADD CONSTRAINT outlets_name_not_blank CHECK (btrim(name) <> '' AND length(name) <= 100),
  -- WEB is the web order sequence's prefix (shifts-spec).
  ADD CONSTRAINT outlets_order_prefix_format CHECK (order_prefix ~ '^[A-Z0-9]{1,8}$' AND order_prefix <> 'WEB'),
  ADD CONSTRAINT outlets_address_length CHECK (length(address) <= 500),
  ADD CONSTRAINT outlets_phone_length CHECK (length(phone) <= 30);
