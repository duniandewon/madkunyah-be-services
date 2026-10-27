# MadKunyah — Tech Stack, Layout and Roadmap

Split out of `context/project-overview.md`. Where this disagrees with `context/coding-standards.md`, coding-standards wins.

## 🧱 Tech Stack

|Category|Choice|
|---|---|
|Language|**Go 1.23+**|
|HTTP router|`chi` (stdlib-compatible, minimal magic)|
|Database|PostgreSQL 16|
|Query layer|`pgx` (`pgxpool`) directly — raw parameterized SQL in each domain's repository, rows scanned manually. No ORM, no `sqlc`|
|Migrations|`golang-migrate`|
|Redis|OTP codes, refresh tokens, idempotency keys, catalog cache — **AOF persistence on**|
|Background jobs|Asynq (Redis-backed) — outbox publisher, order expiry, report rollups|
|Auth|Passwordless OTP → JWT access + rotating refresh, separate audiences for staff and customers|
|JWT|`golang-jwt/jwt/v5`, asymmetric signing (ES256) so verification keys can be distributed|
|OTP delivery|WhatsApp Business API (primary), SMTP (fallback), behind a `Notifier` interface|
|Payments|Midtrans (dynamic QRIS) behind a `PaymentGateway` interface|
|File storage|S3-compatible (product images, receipt PDFs)|
|Validation|`go-playground/validator`|
|Rate limiting|Redis token bucket, applied hardest on OTP endpoints|
|Config|`viper` or a plain typed env struct|
|Logging|`log/slog`, structured JSON, request-scoped|
|Docs|OpenAPI 3 spec|
|Testing|`testify` + `testcontainers-go` (real Postgres + Redis)|
|Monitoring|Sentry + Prometheus metrics (later)|
|Deployment|Docker + Docker Compose; VPS or managed container host|

### Project Layout

```
cmd/
  api/            # HTTP server entrypoint
  worker/         # Asynq worker entrypoint
internal/
  domain/         # entities, value objects, business rules — no framework imports
  auth/           # OTP, JWT issuing, refresh rotation, device enrollment
  order/          # handler.go + service.go + repository.go, per domain
  catalog/
  payment/
  inventory/
  shift/
  report/
  middleware/     # auth, idempotency, rate limit, request logging
platform/
  postgres/       # pgxpool wrapper, InTx, migration runner
  redis/          # otp store, token store, idempotency store
  midtrans/
  notifier/       # whatsapp, smtp
  outbox/
migrations/
api/openapi.yaml
```

---

## 🧭 Roadmap

### MVP — one outlet transacting end to end

- Outlets, devices, staff, roles
- OTP auth + JWT sessions + device enrollment
- Catalog with variants and modifiers
- Shifts, order sequences, offline-safe order creation
- Orders with per-line fulfillment type
- POS payment recording + Midtrans online QRIS
- Shift settlement with per-tender reconciliation
- Daily and range sales reports

### Phase 2 — multi-outlet and control

- Per-outlet pricing and availability
- Item-level inventory with movement ledger
- Void/discount and tender-variance reporting
- Customer accounts and order history
- Refund approval workflow with transfer reference capture
- COGS and margin reporting
- WhatsApp order status notifications

### Phase 3 — depth

- Kitchen display feed (order event stream already in place)
- Ingredient-level recipes → precise COGS and waste tracking
- Vouchers, promo rules engine, loyalty
- Delivery and delivery zones
- Marketplace integrations (GoFood / GrabFood)
- Public API and webhooks for third parties
