# FactorFlow

AI-powered marketplace for tokenized invoices — ETHOnline 2026 submission.

FactorFlow converts verified receivables into compliant tokenized assets, prices them with
privacy-preserving AI and deterministic risk models, and allocates them through a
constraint-aware batch auction.

> **Prototype disclaimer.** Testnet only. Synthetic invoices, no real money, no legal
> assignment of receivables, no production KYC/AML. "Compliance controls" here means
> programmable transfer restrictions, not legal compliance.

## What works today

The deterministic core is complete and runs end to end against PostgreSQL:

```
create invoice → attach encrypted document → queue confidential assessment
              → confidential workflow → live market snapshot → PD/LGD/EL and reserve price
              → constrained batch auction → verified allocation certificate
```

Verified by running it: an invoice created over the API reaches `ASSESSED` with a stored
price bound to the market snapshot and the workflow commitment it came from, delivered
through the outbox by the background worker.

| Area | State |
|---|---|
| Money and rate arithmetic | Integer minor units, exact decimals, banker's rounding |
| Invoice lifecycle | Full state machine, optimistic concurrency, retry per failed stage |
| Risk model `risk-v1` | Published coefficients, decimal sigmoid, clamps, reproducible |
| Pricing | Benchmark + risk/liquidity/concentration premiums, reserve price |
| Market data | Weighted median benchmark, winsorization, TTL, hashed snapshots |
| Batch auction | Min-cost max-flow, minimum lots, exposure repair, allocation certificate |
| Independent verifier | Recomputes every constraint; 20 tampering cases covered |
| Persistence | Schema, repositories, transactional outbox, idempotent writes |
| HTTP API | Invoice endpoints, RFC 9457 problems, trace ids, security headers |
| Confidential workflow | Port plus a deterministic in-process implementation |

Not implemented yet: wallet authentication, Hedera ATS tokenization, settlement saga,
x402 paid endpoints, the live Graph gateway adapter, the live CRE client, and the React
frontend. Every one of those sits behind a port that the in-process implementation already
satisfies, so the path they plug into is the path the tests exercise.

## Architecture

A modular monolith: one Go process, one PostgreSQL database, hard package boundaries.

```
cmd/factorflow/            process entrypoint and wiring
internal/app/              orchestration across modules (the assessment worker)
internal/organization/     issuer/investor profiles, demo eligibility
internal/invoice/          invoice aggregate, state machine, service, endpoints
internal/risk/             deterministic PD/LGD/EL scoring, pricing, workflow port
internal/marketdata/       The Graph snapshots and benchmark normalization
internal/auction/          lots, constrained bids, solver, verifier, certificate
internal/platform/         money, apperr, postgres, outbox, idempotency, httpserver, config
migrations/                goose schema, embedded in the binary
```

Two rules are enforced by `internal/architecture_test.go` rather than by review: the
platform never imports a domain module, and a domain module never imports another one.
The single deliberate exception is `auction → risk`, because a risk grade is the risk
model's published vocabulary and an investor bids "at most grade C"; the reverse would let
pricing depend on demand.

Every external system (Hedera, The Graph, Chainlink CRE, LLM, x402) sits behind an
interface with an in-process implementation, chosen in `cmd/factorflow` by whether
credentials are configured. That is what lets the whole path be exercised offline.

## Requirements

- Go 1.24+
- Docker (integration tests and the local database)
- Node 22+ (frontend, not started yet)

## Commands

```bash
make test          # unit and property tests, no Docker needed
make test-all      # adds integration tests against a real PostgreSQL
make lint          # go vet and golangci-lint
make build         # build ./bin/factorflow
make up            # local PostgreSQL via Docker Compose
```

Integration tests run against a real database, never a mock: half of what a repository
does lives in SQL, constraints and transactions. They start a PostgreSQL container through
testcontainers, or use an existing database when one is named:

```bash
FF_TEST_DATABASE_URL=postgres://factorflow:factorflow@localhost:5432/factorflow?sslmode=disable make test-all
```

With neither a Docker daemon nor that variable, the integration tests skip rather than
fail, so `make test` still works on a machine without Docker.

## Running the demo path locally

```bash
make up && make run
```

The server migrates the schema on start. In development only, the `X-Demo-Organization`
header names the acting organization, because wallet authentication is not implemented yet;
in staging or production there is no resolver at all and protected routes answer 401.

Seed an issuer, then walk the path:

```bash
curl -X POST localhost:8080/api/v1/invoices \
  -H 'Content-Type: application/json' \
  -H "X-Demo-Organization: $ORG" -H 'Idempotency-Key: create-1' \
  -d '{"debtor_ref":"ACME Logistics GmbH","number":"INV-2026-0042","face":"10000.00",
       "currency":"USD","issued_at":"2026-09-06T00:00:00Z","due_at":"2026-11-05T00:00:00Z"}'
```

`POST /invoices/{id}/document` records the encrypted upload, `POST /invoices/{id}/assess`
queues the confidential assessment, and the outbox worker fills in the score and the price
within a second.

## Configuration

Copy `.env.example` to `.env`. Every provider variable is optional: an empty value selects
the in-process implementation. A staging or production deployment refuses to start on the
development database default, so a shared server cannot quietly run against localhost with
the demo header enabled.

## Known gaps

- Clearing 500 invoices against 2000 bids takes about 31 seconds against the
  specification's 2 second target. `BenchmarkClearTargetBatch` measures it; the cost is
  re-solving the flow once per repair round, and a warm-started or network-simplex solver
  is the fix.
- Wallet authentication, the chain adapters and the paid endpoints are ports without live
  implementations, as listed above.

## Documentation

`docs/` carries the technical specification this repository implements.
