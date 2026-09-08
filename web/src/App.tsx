import { NavLink, Route, Routes } from "react-router-dom";

import { AuctionDetail } from "./screens/AuctionDetail";
import { InvoiceDetail } from "./screens/InvoiceDetail";
import { Invoices } from "./screens/Invoices";
import { Market } from "./screens/Market";
import { Marketplace } from "./screens/Marketplace";
import { participants, useSession } from "./session";

export function App() {
  return (
    <div className="app">
      <Masthead />
      <main className="page">
        <Routes>
          <Route path="/" element={<Marketplace />} />
          <Route path="/auctions/:id" element={<AuctionDetail />} />
          <Route path="/invoices" element={<Invoices />} />
          <Route path="/invoices/:id" element={<InvoiceDetail />} />
          <Route path="/market" element={<Market />} />
          <Route path="*" element={<p className="empty">That page does not exist.</p>} />
        </Routes>
      </main>
    </div>
  );
}

/**
 * The masthead carries the participant switcher.
 *
 * Which organization you are acting as changes what every screen is allowed to show, so it
 * belongs beside the navigation rather than buried in a settings page.
 */
function Masthead() {
  const { actor, setActor } = useSession();

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
        <NavLink to="/market" className={({ isActive }) => (isActive ? "is-current" : "")}>
          Market data
        </NavLink>
      </nav>

      <div className="identity">
        <label htmlFor="actor">Acting as</label>
        <select id="actor" value={actor.id} onChange={(event) => setActor(event.target.value)}>
          {participants.map((participant) => (
            <option key={participant.id} value={participant.id}>
              {participant.name} · {participant.role.toLowerCase()}
            </option>
          ))}
        </select>
      </div>
    </header>
  );
}
