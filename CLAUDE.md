# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Go backend for MadKunyah, a multi-outlet F&B business: one API serving the online shop, the in-store POS, and the owner dashboard. The full product spec (scope decisions, data model DDL, auth flow, order lifecycle, API surface, roadmap) is in `context/project-overview.md`. Read the relevant section of it before building any feature. The repo is at an early stage: only the HTTP skeleton, config, and Postgres/Redis connections exist so far.

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
go test ./...                 # no tests exist yet
go test ./internal/httpx -run TestName   # single test
```

Config is loaded from env vars (`.env` is read automatically via godotenv). Required: `DATABASE_URL`, `JWT_PRIVATE_KEY`, `JWT_PUBLIC_KEY` (PEM-encoded ECDSA P-256), `OTP_PEPPER`. Everything else has defaults in `internal/config/config.go`. Startup fails fast if Postgres or Redis can't be pinged.

Health endpoints: `GET /health` (liveness), `GET /ready` (checks DB + Redis).
