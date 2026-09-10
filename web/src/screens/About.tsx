import { Link } from "react-router-dom";

/**
 * The public face of the project.
 *
 * Everything else in this application is behind a wallet, and it should be: nothing in the
 * venue is public. The consequence is that somebody arriving at the address for the first
 * time — a judge, a partner, an investor being shown it — met a sign-in screen and no
 * explanation. This page is the explanation, it is the root of the site, and it is
 * deliberately the only route that renders without a session.
 *
 * It says what is real and what is not in the same breath. A prototype that is vague about
 * which of its integrations are live is asking to be believed; one that names the simulated
 * one can be checked, which is worth more.
 */
export function About() {
  return (
    <div className="about">
      <Header />
      <main>
        <Hero />
        <Numbers />
        <Path />
        <Integrations />
        <Questions />
        <Inside />
        <Evidence />
      </main>
      <Footer />
    </div>
  );
}

function Header() {
  return (
    <header className="about-bar">
      <span className="brand">
        <span className="mark" aria-hidden="true">
          F
        </span>
        FactorFlow
      </span>

      <nav className="about-nav">
        <a href="#path">How it works</a>
        <a href="#live">What is live</a>
        <a href="#faq">Questions</a>
        <a href="#inside">Inside</a>
      </nav>

      <Link className="button is-primary" to="/marketplace">
        Enter the venue
      </Link>
    </header>
  );
}

function Hero() {
  return (
    <section className="about-hero">
      <span className="about-eyebrow">ETHOnline 2026 · prototype on testnet</span>
      <h1>
        Invoices, priced without being read.
        <br />
        Sold in an auction anyone can recheck.
      </h1>
      <p className="about-lede">
        A business is owed money in ninety days and needs it now. FactorFlow takes that
        invoice, prices it without anyone at the platform being able to read it, mints it as
        an asset on Hedera, and sells it through an auction whose result anybody can
        recompute. When the debtor finally pays, the money is divided among the holders to
        the last cent.
      </p>

      <div className="about-cta">
        <Link className="button is-primary" to="/marketplace">
          Enter the venue
        </Link>
        <a className="button" href="#faq">
          The awkward questions
        </a>
      </div>

      <p className="notice about-disclaimer">
        <strong>This is a prototype.</strong> Testnet only. The invoices are synthetic, no
        receivable is legally assigned, and there is no production KYC or AML. The
        transactions on Hedera are real transactions on a real network carrying no real
        money — which is the point of a testnet, and is stated here rather than left to be
        discovered.
      </p>
    </section>
  );
}

const numbers = [
  { value: "0", label: "documents this platform can open" },
  { value: "50", label: "HTTP endpoints, all problem-typed" },
  { value: "700", label: "tests across the server and the browser" },
  { value: "1", label: "process, beside one database" },
];

function Numbers() {
  return (
    <section className="about-numbers">
      {numbers.map((item) => (
        <div key={item.label} className="about-number">
          <span className="num">{item.value}</span>
          <span className="faint small">{item.label}</span>
        </div>
      ))}
    </section>
  );
}

const path = [
  {
    title: "Prove a wallet, not a password",
    body: "The server issues a one-shot challenge, the wallet signs it, and the recovered address names the organization. Nothing is typed and there is no account to lose.",
  },
  {
    title: "Upload a receivable, sealed in the browser",
    body: "The document is encrypted with AES-GCM under a key generated in that tab and never sent. What the platform stores is ciphertext and the digest it computed over the bytes it actually received.",
  },
  {
    title: "Assess it where it can be read",
    body: "A Chainlink CRE workflow inside an attested enclave checks the digest, opens the document, checks its arithmetic against its own line items, and returns six features, a confidence and a commitment. No text, no debtor, no amounts.",
  },
  {
    title: "Price it against a live market",
    body: "A published risk model turns those features into PD, LGD and expected loss. The benchmark is read from lending markets on The Graph, and the snapshot records which subgraphs answered at which block.",
  },
  {
    title: "Mint one token per receivable",
    body: "On Hedera, supply is the face value in minor units, so a holder's balance is the notional they own in cents with nothing to convert and get wrong.",
  },
  {
    title: "Open a batch and take constrained bids",
    body: "An investor bids what they will pay, up to how much, at no worse than grade C, with no more than a share of their money on one debtor.",
  },
  {
    title: "Clear it, and hand over the proof",
    body: "A min-cost max-flow solver allocates the batch and issues a certificate. An independent verifier recomputes every constraint from the inputs — twenty tampering cases are covered by tests.",
  },
  {
    title: "Settle, wait, and divide what arrives",
    body: "Transfers are submitted through a node and confirmed a second time against the public mirror. At maturity the operator records what the debtor actually paid, and it is divided exactly: every unit paid is handed to somebody, none is invented.",
  },
];

function Path() {
  return (
    <section id="path" className="about-section">
      <h2>What actually happens</h2>
      <p className="muted about-sub">
        Eight steps, and the demo dataset is produced by driving them rather than by writing
        rows into a database.
      </p>

      <ol className="about-path">
        {path.map((step, index) => (
          <li key={step.title}>
            <span className="about-step" aria-hidden="true">
              {index + 1}
            </span>
            <div>
              <h3>{step.title}</h3>
              <p className="muted">{step.body}</p>
            </div>
          </li>
        ))}
      </ol>
    </section>
  );
}

const integrations = [
  {
    name: "Hedera testnet",
    state: "live",
    body: "Tokens minted and transferred for real, then confirmed against the public mirror — a second source, which is the only thing that makes a second confirmation worth having.",
  },
  {
    name: "The Graph",
    state: "live",
    body: "Aave V3 and Compound V3 on Ethereum through the decentralized gateway. Two protocols, because a median over one venue is that venue's rate with extra steps.",
  },
  {
    name: "x402 payments",
    state: "live",
    body: "A machine customer is refused with 402 and a price, pays in HBAR with the quote's nonce in the memo, and asks again. No facilitator: the mirror is public.",
  },
  {
    name: "Chainlink CRE",
    state: "simulated",
    body: "The confidential handler compiles to WASM and runs under a TEE constraint in CRE's simulator. This repository has not been granted deployment access, and says so rather than implying otherwise.",
  },
  {
    name: "Language model",
    state: "bounded",
    body: "It writes the sentences under a price it cannot touch. Every figure it uses is checked against what the assessment published, and one that is not costs the whole narration.",
  },
  {
    name: "Document encryption",
    state: "live",
    body: "WebCrypto in the seller's tab. The consequence is deliberate: nobody at the platform can open a stored document, and neither can a backup or a leak.",
  },
];

function Integrations() {
  return (
    <section id="live" className="about-section">
      <h2>What is live, and what is not</h2>
      <p className="muted about-sub">
        Every external system sits behind an interface with an in-process implementation,
        chosen by whether credentials are configured. That is what lets the whole path be
        rehearsed offline — and what makes it worth naming which one is running.
      </p>

      <div className="about-grid">
        {integrations.map((item) => (
          <article key={item.name} className="about-card">
            <header>
              <h3>{item.name}</h3>
              <span className={`pill is-${item.state}`}>{item.state}</span>
            </header>
            <p className="muted">{item.body}</p>
          </article>
        ))}
      </div>
    </section>
  );
}

const questions = [
  {
    q: "Is any of this real money?",
    a: "No, and it is a testnet on purpose. The transactions are genuine — submitted to real nodes, confirmed by a public mirror anyone can read — but the HBAR has no value, the invoices are synthetic, and no receivable has been legally assigned to anybody.",
  },
  {
    q: "So what guarantees the debt is paid?",
    a: "Nothing here does, and any platform that claims otherwise is selling you something. What this does is make the claim legible and priced: what the debtor owes, how likely they are to be late, what that costs, and who is exposed. Default is a first-class outcome rather than an error — a payment short of the face is still divided and still closes the receivable as defaulted.",
  },
  {
    q: "Who can read my invoice?",
    a: "The platform cannot. It is encrypted in your browser before anything is sent, and what is stored is ciphertext plus the digest computed over the bytes actually received — never the digest an uploader claimed. It is opened in exactly one place: an attested enclave that the key is released to, and no request to this platform can produce that key.",
  },
  {
    q: "Then how is it priced?",
    a: "The enclave returns six numbers between zero and one, two verdicts and a commitment. A feature vector cannot be turned back into an invoice, which is why the platform can hold the result in the clear and still be unable to read the document behind it.",
  },
  {
    q: "Does an AI decide the price?",
    a: "No. A published, deterministic model does, with coefficients you can read and arithmetic you can repeat. A language model is asked afterwards, once the price is already stored, and only to write sentences. What it writes is checked against the set of numbers that assessment published; a figure that is not in that set discards the whole narration and a derived explanation takes its place.",
  },
  {
    q: "Why Hedera rather than a chain people have heard of?",
    a: "One receivable is one token whose supply is the face value in minor units, so a balance is a notional in cents. And verification needs no third party and no key: the mirror is public, so a settlement can be checked by somebody who does not trust this platform, was never given an account by it, and is not asking its permission.",
  },
  {
    q: "What is x402 for?",
    a: "The other customer is a program. It wants a price for a receivable this platform has never seen and will not open an account to get one, so it asks, is refused with HTTP 402 and a price, pays, and asks again. Five checks against the mirror decide whether it paid: the transaction succeeded, credited this account, credited enough, came from the payer the proof names, and carries this quote's nonce in its memo.",
  },
  {
    q: "What stops the same invoice being sold twice?",
    a: "A receivable is fingerprinted by its economic identity — which debtor owes how much, under which number, by when — and the venue allows one live receivable per fingerprint. The seller is deliberately not part of that fingerprint: the fraud factoring actually suffers from is the same paper sold to two financiers, and a fingerprint that included the seller would agree with both of them.",
  },
  {
    q: "Doesn't approving participants defeat the point of a blockchain?",
    a: "Eligibility here is a venue rule, not a state one: an operator admits a wallet, and that is a decision about who may trade in this market. No passport is asked for and no identity is stored. A real financing platform would have legal obligations that this prototype does not pretend to satisfy, and the honest statement is that the two are different problems rather than that one solves the other.",
  },
  {
    q: "Who actually holds the tokens?",
    a: "The platform's treasury, and that is custody — named as such rather than dressed up. A token needs a treasury to sign for it, and this platform does not hold an issuer's key. A production system would have each investor bring their own account; the demo opens one per investor and holds those keys, which is a real limitation and is written down as one.",
  },
  {
    q: "How is the money split when the debtor pays?",
    a: "Exactly, in minor units. Each investor whose transfer completed gets their share, the issuer gets whatever it never sold, and the remainder goes to the largest fractional parts with ties broken by party id — so the same payment splits the same way on every machine that computes it. A receivable is repaid once; a retried request returns the payment already recorded rather than crediting everyone twice.",
  },
  {
    q: "Can I check the auction was cleared honestly?",
    a: "Yes, and without asking the platform. Clearing produces an allocation certificate, and an independent verifier recomputes every constraint from the inputs. Twenty ways of tampering with an allocation are covered by tests, each of which has to be caught rather than merely noticed.",
  },
];

function Questions() {
  return (
    <section id="faq" className="about-section">
      <h2>The awkward questions</h2>
      <p className="muted about-sub">
        The ones worth asking are the ones a prototype usually avoids, so they are the ones
        answered here.
      </p>

      <div className="about-grid is-faq">
        {questions.map((item) => (
          <article key={item.q} className="about-card">
            <h3>{item.q}</h3>
            <p className="muted">{item.a}</p>
          </article>
        ))}
      </div>
    </section>
  );
}

function Inside() {
  return (
    <section id="inside" className="about-section">
      <h2>Inside</h2>
      <p className="muted about-sub">
        A modular monolith: one Go process, one PostgreSQL database, and package boundaries
        enforced by a test rather than by review.
      </p>

      <div className="about-grid is-three">
        <article className="about-card">
          <h3>Money is never a float</h3>
          <p className="muted">
            Every amount is an integer in minor units with an exact decimal rate beside it. A
            price is computed the way a settlement clerk would, and a cent that goes missing
            is a failing test rather than a rounding convention.
          </p>
        </article>
        <article className="about-card">
          <h3>Writes survive being retried</h3>
          <p className="muted">
            Idempotency keys, optimistic concurrency, a transactional outbox, and an audit
            trail written inside the caller's transaction — so what happened and what was
            recorded cannot disagree.
          </p>
        </article>
        <article className="about-card">
          <h3>Boundaries are compiled, not agreed</h3>
          <p className="muted">
            The platform layer never imports a domain module and a domain module never
            imports another. There is one deliberate exception, and the reason it exists is
            written next to it.
          </p>
        </article>
      </div>

      <pre className="about-tree">{`cmd/factorflow/       process entrypoint, the seed, the machine customer
internal/app/         orchestration: assessment, issuance, marketplace, collections
internal/invoice/     the aggregate, its state machine, its endpoints
internal/risk/        deterministic scoring, pricing, the confidential port
internal/auction/     lots, constrained bids, solver, verifier, certificate
internal/settlement/  the chain saga and its second confirmation
internal/redemption/  what the debtor paid, divided exactly
internal/platform/    money, errors, postgres, outbox, idempotency, config
cre-workflows/        the handler that runs inside the enclave
web/                  React, TypeScript, decimals rendered without floats`}</pre>
    </section>
  );
}

const HASHSCAN = "https://hashscan.io/testnet/account/";

function Evidence() {
  return (
    <section className="about-section">
      <h2>Check it yourself</h2>
      <p className="muted about-sub">
        These accounts are public. Nothing below asks you to take this platform's word for
        anything it did.
      </p>

      <div className="about-grid">
        <article className="about-card">
          <h3>The platform's account</h3>
          <p className="muted">
            Mints the receivables and receives what machine customers pay. Every settlement
            this venue has made is in its transaction history.
          </p>
          <a className="hash" href={`${HASHSCAN}0.0.10419676`} target="_blank" rel="noreferrer">
            0.0.10419676 ↗
          </a>
        </article>
        <article className="about-card">
          <h3>The machine customer</h3>
          <p className="muted">
            A program with its own account and no relationship to this platform beyond
            paying it. Its transfers carry the nonce of the quote they bought.
          </p>
          <a className="hash" href={`${HASHSCAN}0.0.10456604`} target="_blank" rel="noreferrer">
            0.0.10456604 ↗
          </a>
        </article>
        <article className="about-card">
          <h3>The source</h3>
          <p className="muted">
            Including the enclave handler, the solver, the verifier and the tests that hold
            each claim on this page to its word.
          </p>
          <a
            className="hash"
            href="https://github.com/GoldFridge/factorflow"
            target="_blank"
            rel="noreferrer"
          >
            github.com/GoldFridge/factorflow ↗
          </a>
        </article>
      </div>
    </section>
  );
}

function Footer() {
  return (
    <footer className="about-foot">
      <span className="faint small">
        FactorFlow · ETHOnline 2026 · a prototype on Hedera testnet, with synthetic invoices
        and no legal assignment of anything.
      </span>
      <Link className="button" to="/marketplace">
        Enter the venue
      </Link>
    </footer>
  );
}
