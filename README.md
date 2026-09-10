# FactorFlow

AI-powered marketplace for tokenized invoices — ETHOnline 2026 submission.

FactorFlow converts verified receivables into compliant tokenized assets, prices them with
privacy-preserving AI and deterministic risk models, and allocates them through a
constraint-aware batch auction.

> **Prototype disclaimer.** Testnet only. Synthetic invoices, no real money, no legal
> assignment of receivables, no production KYC/AML. "Compliance controls" here means
> programmable transfer restrictions, not legal compliance.

## What works today

The whole path runs end to end against PostgreSQL, and the demo dataset is produced by
driving it rather than by writing rows:

```
sign in with a wallet → upload a receivable, encrypted in the browser
                     → confidential assessment against a live market snapshot
                     → PD/LGD/EL and a reserve price → approve → mint on Hedera testnet
                     → open a batch → constrained bids → verified allocation certificate
                     → settle the transfers → the debtor pays → the money is divided
```

| Area | State |
|---|---|
| Money and rate arithmetic | Integer minor units, exact decimals, banker's rounding |
| Invoice lifecycle | Full state machine, optimistic concurrency, retry per failed stage |
| Risk model `risk-v1` | Published coefficients, decimal sigmoid, clamps, reproducible |
| Pricing | Benchmark + risk/liquidity/concentration premiums, reserve price |
| Market data | **Live** Ethereum lending markets through The Graph gateway, hashed snapshots |
| Batch auction | Min-cost max-flow, minimum lots, exposure repair, allocation certificate |
| Independent verifier | Recomputes every constraint; 20 tampering cases covered |
| Settlement | Saga with submit / consensus / mirror / account steps and reconciliation |
| Maturity | Repayment recorded by an operator, divided exactly among the holders |
| Tokenization | **Live** minting on Hedera testnet through the HTS adapter |
| Documents | AES-GCM in the browser; the platform stores bytes it cannot read |
| Identity | Wallet sign-in over EIP-191, multi-wallet choice over EIP-6963 |
| Disclosure | An offered receivable is readable by the venue; an unlisted one is not |
| Persistence | Schema, repositories, transactional outbox, idempotent writes |
| HTTP API | 41 endpoints, RFC 9457 problems, trace ids, security headers |
| Web | React + TypeScript, exact decimals rendered without floats |

Still behind in-process ports rather than live services: the confidential workflow (Chainlink
CRE), the x402 paid endpoints, and the chain transfer executor — settlement moves assets on
the local ledger even when minting is live on Hedera.

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

It proxies `/api` to the server. Signing in is a wallet signature: the server issues a
one-shot challenge, the wallet signs it, and the recovered address names the organization —
the browser never holds a key, and the session cookie it gets back is HttpOnly. A wallet with
no organization behind it is offered registration rather than an error, because the refusal
only arrives after the signature has proved who is asking.

Wallets are discovered through EIP-6963 rather than through `window.ethereum`. Every injected
wallet used to claim that one global, so with two installed the last to load won and the
other was unreachable — someone with MetaMask and Binance Wallet could find themselves
permanently connected to whichever injected second. Each wallet now announces itself, the
sign-in screen lists them, and the choice is the person's.

A browser with no injected wallet falls back to naming a seeded participant in a header the
API honours only in development. Even then the server reads what that organization may do
from its own record, so the shortcut can never grant more than a real session would.

## Configuration

Copy `.env.example` to `.env`. Every provider variable is optional: an empty value selects
the in-process implementation. A staging or production deployment refuses to start on the
development database default, so a shared server cannot quietly run against localhost with
the demo header enabled.

## Hedera

With `FF_HEDERA_ACCOUNT_ID` and `FF_HEDERA_PRIVATE_KEY` set, an approved receivable is minted
as a fungible token on the configured network — testnet unless something says otherwise. One
receivable is one token, and its supply is the face value in minor units, so a holder's
balance is the notional they own in cents with no conversion to get wrong. The token's memo
carries the commitment to the invoice and nothing else.

The treasury is the platform's own account rather than the issuer's wallet. That is a real
limitation, not a shortcut: a treasury signs the transactions that create and move its
tokens, and the platform does not hold an issuer's key. So the supply is minted into custody,
and the issuer's claim until settlement is the record in this system rather than a balance on
the network. Moving a token to an investor additionally needs that investor's account to have
associated it, which is the investor's decision and not something this platform can perform
for them — so settlement transfers still run through the local executor.

## Market data

With `FF_GRAPH_API_KEY` set, the benchmark is read from lending markets on The Graph's
decentralized network — Aave V3 and Compound V3 on Ethereum by default, both publishing the
same standardized schema, so one query text serves both. The snapshot records which subgraphs
answered and at which block, which is what lets a published price be traced back to the rows
behind it rather than taken on trust.

Two protocols rather than one: a median over a single venue is that venue's rate with extra
steps. A subgraph that fails takes the whole fetch with it, because dropping the venue that
did not answer would move the benchmark with nothing on the snapshot to say why — so pricing
fails closed and the invoice is assessed once the gateway is answering again.

## Uploading a receivable

The document is encrypted in the browser before anything is sent: AES-GCM under a key
generated in that tab, which is never transmitted and is kept only there. What the platform
stores is the ciphertext and the digest it computed over the bytes it actually received —
never the digest the uploader claimed — and that digest is what binds the published price to
this document.

The consequence is deliberate and worth stating plainly: nobody at the platform can open a
stored document, and neither can a backup or a leak. With a confidential workflow in front of
it the key would be wrapped to the enclave's public key instead, which is the one party meant
to read a document, and still not this server.

## What a bidder may read

An invoice belongs to its issuer: `GET /invoices/{id}` answers "not found" to everyone else,
and the refusal deliberately does not distinguish a receivable that is not yours from one
that does not exist.

Offering the paper in a batch changes that question. An investor is asked for money against
terms, and terms nobody may read are not a market, so `GET /listings/{invoiceID}` discloses
what was put on the board: the terms of the lot, the published price with its decomposition,
the market snapshot the benchmark came from, and the commitment tying that score to the
document. It stops there — no document, no object key, no history, no version — and it opens
only once the batch has left DRAFT, because a draft can still be abandoned without anyone
ever having been asked to price it. In the interface this is the "why this price" link on the
board, and a stranger who lands on `/invoices/{id}` is handed over to it.

## When the receivable comes due

Everything before maturity is a promise. `POST /invoices/{id}/repayment` records what the
debtor actually paid and divides it among the parties that hold the receivable: each
investor whose transfer completed, and the issuer for whatever it never sold. The division
is exact in minor units — every unit paid is handed to somebody, none is invented — and the
remainder goes to the largest fractional parts with ties broken by party id, so the same
payment splits the same way on every machine that computes it.

Only an operator records it, because in factoring the debtor pays the platform: letting the
seller declare that the money arrived would let it decide when its own obligation ended. A
payment short of the face is still divided and still closes the receivable as DEFAULTED,
because an investor reading its position has to be able to tell a receivable that came good
from one that did not. `POST /invoices/{id}/default` closes one the debtor never paid, and
refuses before the due date.

A receivable is repaid once: the unique constraint on the repayment says so, and a retried
request returns the payment already recorded rather than crediting every holder twice.
`GET /repayments` is a party's own record of what came back, and the Returns screen is that
list with the reader's own share of each payment rather than the payment's total.

The seeded demo ends here too: the financed receivable's debtor pays, so the finished story
runs from an encrypted upload to money divided among the parties that held the paper.

## Reseeding

Stop the server before seeding. Both processes run the same outbox dispatcher, and a running
server will happily claim the seed's work — which is correct behaviour for competing
consumers, and confusing when the two were started with different configuration.

## What the model is allowed to do

A language model writes the sentences under a published price and cannot touch the price
itself. By the time it is asked, the assessment is stored: the score, the grade, the
premiums and the reserve price already exist and are what the venue prices against.

The boundary is enforced rather than requested. What the model writes is checked against the
set of numbers that assessment published, in the shapes a writer would use them — 0.036325,
3.63%, 3.6 — and a figure that is not in that set costs the whole narration, not the
sentence that carried it. What replaces it is a narration this codebase derives from the
same contributions, which is what a reader sees whenever no model answered, the call timed
out, or what came back cited something nobody published.

The prompt is also the whole of what the model may see: a feature vector, six weighted
contributions and the prices those produced. No document text, no invoice number, no debtor,
no party. The privacy rule is not enforced by asking politely — the content is not there.

Set `FF_LLM_API_KEY` (and optionally `FF_LLM_BASE_URL`, `FF_LLM_MODEL`, which default to
DeepSeek) to turn it on. Without a key, every assessment is explained from its own
coefficients, and the screen says which of the two it is showing.

## Double financing

A receivable is fingerprinted by its economic identity — which debtor owes how much, under
which number, by when — and the venue allows one live receivable per fingerprint. The
issuer is deliberately not part of it: the fraud factoring actually suffers from is the same
paper sold to two financiers, and a fingerprint that included the seller would agree with
both of them.

A receivable that was rejected, matured or defaulted releases its terms, because whatever it
referred to has either been paid or written off. Production needs the debtor's confirmation
or a receivables registry; a hash of the terms is the honest demo version of that, and the
refusal says which facts collided rather than only "conflict".

## Deploying it

The process serves both the API and the interface, so a deployment is one container beside
a database. `FF_WEB_DIR` points at the built application; without it the process is an API
and nothing else.

On the server, with a domain pointing at it:

```
cp .env.example .env          # then fill in the keys and set DOMAIN and POSTGRES_PASSWORD
docker compose -f deploy/docker-compose.prod.yml build
docker compose -f deploy/docker-compose.prod.yml run --rm app seed
docker compose -f deploy/docker-compose.prod.yml run --rm app operator 0xYourWalletAddress
docker compose -f deploy/docker-compose.prod.yml up -d
```

Caddy gets its own certificate, which is not decoration: the session cookie is marked Secure
outside development, so over plain HTTP nobody could stay signed in.

The seed runs as its own container rather than inside the running one. Both the server and
the seeder drive the same outbox, so a seed racing a server is how a demo ends up with data
that came from two different builds.

The `operator` command exists because the API cannot provide it. An operator is the party
that admits everyone else, so the first one cannot be admitted by anybody, and registration
deliberately refuses to mint one — on a fresh server the only operator would otherwise be
the seed's, whose address is a placeholder nobody holds.

`FF_AUTO_APPROVE=true` admits a wallet the moment it registers. It is off outside
development, because eligibility is a decision somebody is meant to take; a public demo is
the honest exception, since a judge who registers at midnight has nobody to admit them.

## Known gaps

- The money leg is not on chain. Minting is live on Hedera testnet, but an investor pays
  and is paid off chain: there is no escrow under a bid, no delivery-versus-payment, and no
  burn at redemption. The blocker is not the adapter — it is that the seeded investors are
  `0x` addresses with no Hedera account, so nothing can be transferred to them.
- Clearing 500 invoices against 2000 bids takes about 31 seconds against the
  specification's 2 second target. `BenchmarkClearTargetBatch` measures it; the cost is
  re-solving the flow once per repair round, and a warm-started or network-simplex solver
  is the fix.
- The confidential workflow and the paid endpoints run in process. Both are ports with
  working in-process implementations, so the path they plug into is the path the tests
  exercise.
- No double-pledge registry: nothing yet stops the same document being sold twice under two
  invoices, which the stored commitment already makes detectable.
- No operator screen beyond the one action maturity needs, and no deployment.

## Documentation

`docs/` carries the technical specification this repository implements.
