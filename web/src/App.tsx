import { NavLink, Route, Routes } from "react-router-dom";

import { AuctionDetail } from "./screens/AuctionDetail";
import { InvoiceDetail } from "./screens/InvoiceDetail";
import { Listing } from "./screens/Listing";
import { Invoices } from "./screens/Invoices";
import { Market } from "./screens/Market";
import { Marketplace } from "./screens/Marketplace";
import { Operator } from "./screens/Operator";
import { Portfolio } from "./screens/Portfolio";
import { NewInvoice } from "./screens/NewInvoice";
import { SignIn } from "./screens/SignIn";
import { useSession } from "./session";

export function App() {
  const { stage } = useSession();

  if (stage === "loading") {
    return (
      <div className="app">
        <p className="empty">Checking your session…</p>
      </div>
    );
  }

  // Nothing behind the gate is public, and the server would refuse it anyway; showing empty
  // screens to someone who is not signed in only makes the refusal look like a bug.
  if (stage !== "signed-in") {
    return (
      <div className="app">
        <SignIn />
      </div>
    );
  }

  return (
    <div className="app">
      <Masthead />
      <main className="page">
        <Waiting />
        <Routes>
          <Route path="/" element={<Marketplace />} />
          <Route path="/auctions/:id" element={<AuctionDetail />} />
          <Route path="/invoices" element={<Invoices />} />
          <Route path="/invoices/new" element={<NewInvoice />} />
          <Route path="/invoices/:id" element={<InvoiceDetail />} />
          <Route path="/listings/:id" element={<Listing />} />
          <Route path="/portfolio" element={<Portfolio />} />
          <Route path="/market" element={<Market />} />
          <Route path="/operator" element={<Operator />} />
          <Route path="*" element={<p className="empty">That page does not exist.</p>} />
        </Routes>
      </main>
    </div>
  );
}

/**
 * Waiting says why nothing works yet, to the one person who cannot tell.
 *
 * A registered wallet can sign in before an operator has admitted it, and the venue then
 * reads normally and refuses every action. Without this the refusals look like faults.
 */
function Waiting() {
  const { actor, stage } = useSession();

  if (stage !== "signed-in" || actor.eligible) {
    return null;
  }
  return (
    <div className="notice" role="status">
      <strong>{actor.name}</strong> is registered and waiting to be admitted. You can look
      around the venue; uploading a receivable, bidding and settling stay closed until the
      platform's operator approves this wallet.
    </div>
  );
}

/**
 * The masthead carries who you are acting as and how to stop.
 *
 * The address is shown abbreviated beside the organization's name, because in a venue where
 * identity is a key, the name is what a person recognises and the address is what they
 * verify.
 */
function Masthead() {
  const { actor, auth, signOut, busy, isOperator } = useSession();

  return (
    <header className="masthead">
      <span className="brand">
        <span className="mark" aria-hidden="true">
          F
        </span>
        FactorFlow
      </span>

      <nav className="nav">
        <NavLink to="/" className={({ isActive }) => (isActive ? "is-current" : "")} end>
          Marketplace
        </NavLink>
        <NavLink to="/invoices" className={({ isActive }) => (isActive ? "is-current" : "")}>
          Receivables
        </NavLink>
        <NavLink to="/portfolio" className={({ isActive }) => (isActive ? "is-current" : "")}>
          Portfolio
        </NavLink>
        <NavLink to="/market" className={({ isActive }) => (isActive ? "is-current" : "")}>
          Market data
        </NavLink>
        {isOperator ? (
          <NavLink to="/operator" className={({ isActive }) => (isActive ? "is-current" : "")}>
            Operator
          </NavLink>
        ) : null}
      </nav>

      <div className="account">
        {auth ? <span className="tag">development header</span> : null}
        {actor.eligible ? null : <span className="pill is-working">Awaiting approval</span>}

        <span className="account-card">
          <span className="account-name">{actor.name || "Unnamed organization"}</span>
          <span className="account-wallet mono">{abbreviate(actor.wallet)}</span>
        </span>

        <button className="button" disabled={busy} onClick={signOut}>
          Sign out
        </button>
      </div>
    </header>
  );
}

/** abbreviate keeps the ends of an address, which are what a person checks. */
function abbreviate(address: string): string {
  return address.length > 12 ? `${address.slice(0, 6)}…${address.slice(-4)}` : address;
}
