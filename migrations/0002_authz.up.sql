-- Audit trail for privileged actions. Append-only: rows are never updated or
-- deleted. Role-grant history (needed to judge offline operations against the
-- roles in effect at the time) is derived from role.granted / role.revoked.
CREATE TABLE audit_logs (
  id          UUID PRIMARY KEY,
  outlet_id   UUID REFERENCES outlets(id),
  actor_id    UUID,
  actor_type  TEXT NOT NULL,               -- staff | customer | system
  action      TEXT NOT NULL,               -- role.granted | role.revoked | staff.deactivated | ...
  entity_type TEXT NOT NULL,
  entity_id   UUID NOT NULL,
  value_delta BIGINT,                      -- money moved, for fraud reporting
  reason      TEXT,
  before      JSONB,
  after       JSONB,
  approved_by UUID REFERENCES staff(id),   -- supervisor step-up approver
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_entity_idx ON audit_logs (entity_type, entity_id, created_at);
CREATE INDEX audit_logs_outlet_idx ON audit_logs (outlet_id, created_at);
CREATE INDEX staff_outlet_roles_outlet_idx ON staff_outlet_roles (outlet_id);
