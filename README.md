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
create invoice → attach encrypted document → confidential assessment
              → live market snapshot → PD/LGD/EL and reserve price
              → approve → mint the tokenized asset
              → open an auction → constrained bids → verified allocation certificate
```

Verified by running it against PostgreSQL: an invoice created over the API is assessed and
priced by the background worker, tokenized through the issuer port, listed as a lot at the
price the risk model published, bid on by two investors, and cleared. The financed invoice
ends at `ALLOCATED`, the winning bid at `ALLOCATED`, and the losing one is told which of its
own limits refused the lot.

| Area | State |
|---|---|
| Money and rate arithmetic | Integer minor units, exact decimals, banker's rounding |
| Invoice lifecycle | Full state machine, optimistic concurrency, retry per failed stage |
| Risk model `risk-v1` | Published coefficients, decimal sigmoid, clamps, reproducible |
| Pricing | Benchmark + risk/liquidity/concentration premiums, reserve price |
| Market data | Weighted median benchmark, winsorization, TTL, hashed snapshots |
| Batch auction | Min-cost max-flow, minimum lots, exposure repair, allocation certificate |
| Auction API | Open a batch, bid, clear, read allocations and rejection reasons |
| Independent verifier | Recomputes every constraint; 20 tampering cases covered |
| Persistence | Schema, repositories, transactional outbox, idempotent writes |
| HTTP API | Invoice endpoints, RFC 9457 problems, trace ids, security headers |
| Tokenization | Asset lifecycle, issuer port, in-process issuer |
| Application layer | Assessment and issuance workers, opening and clearing auctions |
| Confidential workflow | Port plus a deterministic in-process implementation |

Not implemented yet: wallet authentication, the Hedera ATS adapter, the settlement saga,
x402 paid endpoints, the live Graph gateway adapter, the live CRE client, and the React
frontend. Every one of those sits behind a port that the in-process implementation already
satisfies, so the path they plug into is the path the tests exercise.

## Architecture

A modular monolith: one Go process, one PostgreSQL database, hard package boundaries.

```
cmd/factorflow/            process entrypoint and wiring
internal/app/              orchestration across modules: assessment, issuance, marketplace
internal/organization/     issuer/investor profiles, demo eligibility
internal/invoice/          invoice aggregate, state machine, service, endpoints
internal/risk/             deterministic PD/LGD/EL scoring, pricing, workflow port
internal/marketdata/       The Graph snapshots and benchmark normalization
internal/auction/          lots, constrained bids, solver, verifier, certificate
internal/tokenization/     tokenized asset, chain lifecycle, issuer port
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

`POST /invoices/{id}/document` records the encrypted upload and `POST /invoices/{id}/assess`
queues the confidential assessment; the outbox worker fills in the score and the price
within a second. Then `approve`, `tokenize`, and `POST /auctions` with the invoice ids opens
the batch. Investors bid at `POST /auctions/{id}/bids`, and `POST /auctions/{id}/clear`
after the closing time returns the allocation and its certificate.

## Running the demo

Three commands, from the repository root:

```
docker compose -f deploy/docker-compose.yml up -d   # PostgreSQL on :5432
go run ./cmd/factorflow seed                        # the demo dataset
go run ./cmd/factorflow                             # the API on :8080
```

The seed builds its dataset by driving the real services, with a clock it moves rather than
rules it relaxes: the finished batch was uploaded, priced, minted, cleared after its window
closed, and paid, a week of history ago. Seeding an already-seeded database does nothing, so
restarting a demo server is safe. Every identifier it writes is derived from a fixed
namespace, so the dataset — grades and prices included — is the same one every time.

The web app is a separate process in development:

```
cd web && npm install && npm run dev                # the app on :5173
```

It proxies `/api` to the server, and the participant switcher in the masthead chooses which
seeded organization you are acting as. That switcher is a development affordance: the server
reads what an organization may actually do from its own record, so choosing a name can only
narrow what the API allows.

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
