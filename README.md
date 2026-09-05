# FactorFlow

AI-powered marketplace for tokenized invoices — ETHOnline 2026 submission.

FactorFlow converts verified receivables into compliant tokenized assets, prices them with
privacy-preserving AI and deterministic risk models, and allocates them through a
constraint-aware batch auction.

> **Prototype disclaimer.** Testnet only. Synthetic invoices, no real money, no legal
> assignment of receivables, no production KYC/AML. "Compliance controls" here means
> programmable transfer restrictions, not legal compliance.

## Architecture

A modular monolith: one Go process, one PostgreSQL database, hard package boundaries.

```
cmd/factorflow/            process entrypoint
internal/identity/         wallet challenge, sessions, RBAC
internal/organization/     issuer/investor profiles, demo eligibility
internal/invoice/          invoice aggregate and state machine
internal/risk/             deterministic PD/LGD/EL scoring and pricing
internal/marketdata/       The Graph snapshots and benchmark normalization
internal/tokenization/     Hedera ATS asset lifecycle
internal/auction/          bids, constraints, solver, allocation certificate
internal/settlement/       transfer saga and on-chain reconciliation
internal/payments/         x402 challenge, verification, response cache
internal/platform/         postgres, httpserver, outbox, telemetry, money
```

Dependency rule: transport and adapters depend on application and domain, never the other
way round. Each domain module owns `domain.go`, `service.go`, `ports.go`, `postgres.go`,
`http.go`. Cycles are rejected by an architecture test.

Every external system (Hedera, The Graph, Chainlink CRE, LLM, x402 facilitator) sits behind
an interface with an in-process fake, so the full path is testable offline. Live adapters are
selected by configuration.

## Requirements

- Go 1.24+
- Docker (integration tests use testcontainers; local stack uses Docker Compose)
- Node 22+ (frontend, added later)

## Commands

```bash
make test          # unit and property tests
make test-all      # adds integration tests that need Docker
make lint          # go vet and golangci-lint
make build         # build ./bin/factorflow
make up            # local PostgreSQL via Docker Compose
```

## Status

Work in progress. See `docs/` for the technical specification this implements.
