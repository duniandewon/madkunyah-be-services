# MadKunyah — Flows and API Surface

Split out of `context/project-overview.md`.

## 🔌 Architecture

```mermaid
graph TD;
  Web[Online Shop] --> API
  POS[POS App] --> API
  Dash[Dashboard] --> API
  API[Go API - chi] --> PG[(PostgreSQL)]
  API --> Redis[(Redis: OTP, refresh, idem)]
  API --> S3[(Object Storage)]
  API --> MT[Midtrans]
  MT -- webhook --> API
  API --> Outbox[(outbox_events)]
  Outbox --> Worker[Asynq Worker]
  Worker --> PG
  Worker --> Notif[WhatsApp / Email]
```

---

## 🔐 Auth Flow

```mermaid
sequenceDiagram
  participant C as Client
  participant API
  participant R as Redis
  participant N as WhatsApp / Email
  
  C->>API: POST /auth/otp/request {identifier}
  API->>R: check cooldown + quota
  API->>R: SET otp:{id} = HMAC(code), TTL 5m
  API->>N: deliver code
  API-->>C: 202 (generic — no existence disclosure)
  
  C->>API: POST /auth/otp/verify {identifier, code}
  API->>R: compare HMAC, increment attempts
  API->>R: DEL otp:{id}, SET refresh:{hash}
  API-->>C: access token (15m) + refresh token
  
  Note over C,API: rotation on every refresh, reuse of a rotated token revokes the family
```

---

## 🔄 Order Lifecycle

```mermaid
stateDiagram-v2
  [*] --> placed: POS "Process order" (numbered)
  placed --> placed: add lines / void a line
  placed --> completed: payment recorded
  placed --> cancelled: cancelled before payment (reason)
  [*] --> pending_payment: web checkout
  pending_payment --> placed: webhook settled
  pending_payment --> expired: QRIS expiry
  completed --> refunded: refund issued
```

There are no server-side drafts: a POS ticket exists only on the device until *Process order*. A POS order is `placed` as an open bill and **payment completes it**; there is no separate handover step. `paid_at` marks payment and is not a status. A paid order is voided only by a holder of `order.void.paid` (Refunds + Paid Void). `preparing` and `ready` are allowed by the schema for kitchen statuses later.

Web orders never enter `placed` on a client callback — only on a verified webhook.

---

## 📶 Offline POS

```mermaid
sequenceDiagram
  participant POS
  participant Q as Local Queue
  participant API
  
  Note over POS: device already enrolled, <br/>cashier unlocks with local PIN
  POS->>Q: create order (UUID generated locally)
  
  Note over POS,Q: network down — sales continue,<br/>access token may expire
  Q->>API: POST /auth/token/refresh (on reconnect)
  API-->>Q: fresh access token
  
  Q->>API: POST /sync/batch (Idempotency-Key per order)
  API-->>Q: allocated order numbers
  
  Note over API: retry returns the SAME number,<br/>never burns a new sequence
```

---

## 🔌 API Surface (indicative)

**Auth (shared shape, separate audiences)**

```
POST   /v1/auth/otp/request              # rate limited hard
POST   /v1/auth/otp/verify               # → access + refresh
POST   /v1/auth/token/refresh            # rotates; reuse revokes family
POST   /v1/auth/logout                   # deletes refresh token
POST   /v1/auth/devices/enroll           # OTP-gated, binds device
POST   /v1/auth/devices/{id}/revoke
POST   /v1/staff/me/pin                  # set local unlock PIN
```

**Public / customer**

```
GET    /v1/outlets                       GET  /v1/outlets/{id}/menu
POST   /v1/orders                        # guest or authenticated
GET    /v1/track/{tracking_token}        # opaque token, never order_number
POST   /v1/orders/{id}/payment           # returns Midtrans QRIS payload
GET    /v1/me/orders                     # accounts only
POST   /v1/webhooks/midtrans
```

**POS (device token + `X-Acting-Staff-Id`; outlet from the token)**

```
GET    /v1/pos/shifts/current            POST /v1/pos/shifts
POST   /v1/pos/shifts/{id}/close
GET    /v1/pos/catalog                   # versioned bundle for offline use
POST   /v1/pos/orders                    # process order; retry-safe by client order id
GET    /v1/pos/orders                    # ?status=placed = open bills of the open shift
GET    /v1/pos/orders/{id}
POST   /v1/pos/orders/{id}/lines         # add lines; retry-safe by line ids
POST   /v1/pos/orders/{id}/lines/{lid}/void
POST   /v1/pos/orders/{id}/cancel
POST   /v1/pos/orders/{id}/payments      # retry-safe by client payment id
POST   /v1/pos/orders/{id}/refunds       # Refunds + Paid Void
POST   /v1/sync/batch                    # replay queued offline operations
```

**Dashboard**

```
GET    /v1/reports/sales?from&to&outlet_id&channel&group_by
GET    /v1/reports/products              GET /v1/reports/employees
GET    /v1/reports/voids-discounts       GET /v1/reports/tender-variance
GET    /v1/reports/shifts/{id}
GET    /v1/outlets/{id}/shifts           GET /v1/outlets/{id}/shifts/{sid}
GET    /v1/outlets/{id}/orders           GET /v1/outlets/{id}/orders/{oid}
CRUD   /v1/products  /v1/categories  /v1/modifiers
CRUD   /v1/staff     /v1/outlets     /v1/devices
GET    /v1/inventory/stock               POST /v1/inventory/movements
GET    /v1/audit-logs                    GET /v1/auth-events
```
