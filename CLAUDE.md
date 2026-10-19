# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Go backend for MadKunyah, a multi-outlet F&B business: one API serving the online shop, the in-store POS, and the owner dashboard. The full product spec (scope decisions, data model DDL, auth flow, order lifecycle, API surface, roadmap) is in `context/project-overview.md`. Read the relevant section of it before building any feature. Built so far: HTTP skeleton, config, Postgres/Redis connections, embedded migrations (applied at startup from `migrations/`), auth slice 1 (OTP login, JWT + rotating refresh sessions) in `internal/auth`, outlet-scoped authorization (`internal/authz`) with staff management endpoints (`internal/staff`), and auth slice 3: POS device enrollment/revocation (`internal/auth`), staff PIN set/reset (`internal/staff`), and the POS roster and PIN-failure endpoints (`internal/pos`).

## Context files

In order to have the full context of this project, make sure to always reference these files:

- @context/project-overview.md
- @context/coding-standards.md
- @context/ai-interaction.md
- @context/current-feature.md

## Commands

```bash
go run ./cmd/api              # run the API (needs Postgres + Redis reachable)
go build -o bin/api ./cmd/api # build (bin/ is gitignored)
go vet ./...
go test ./...                 # unit tests (no integration suite yet)
TEST_DATABASE_URL=... go test ./internal/authz   # permission-matrix test against a migrated DB
go test ./internal/httpx -run TestName   # single test
```

Config is loaded from env vars (`.env` is read automatically via godotenv). Required: `DATABASE_URL`, `JWT_PRIVATE_KEY`, `JWT_PUBLIC_KEY` (PEM-encoded ECDSA P-256), `OTP_PEPPER`. Everything else has defaults in `internal/config/config.go`. Startup fails fast if Postgres or Redis can't be pinged.

Health endpoints: `GET /health` (liveness), `GET /ready` (checks DB + Redis).
