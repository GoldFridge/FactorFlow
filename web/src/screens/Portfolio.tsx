import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";

import { api } from "../api/client";
import type { Holding } from "../api/types";
import { Chips, Discs, Loader, Stat, Status, useAsync } from "../components";
import { compact, date, money, relative, sum } from "../format";
import { useSession } from "../session";

/**
 * What an investor owns, and what it returned.
 *
 * The venue could show what you bid and what came back, and nothing in between — the
 * position itself. This is that: every receivable bought, what was paid for it, when the
 * debtor owes it, and once the debtor has paid, this holder's own share of the money.
 *
 * A transfer still in flight is shown rather than hidden. An investor whose settlement is
 * stuck has committed money, and a venue that stayed silent until the transfer completed
 * would be silent at exactly the moment it matters.
 */
export function Portfolio() {
  const { auth, isInvestor } = useSession();
  const navigate = useNavigate();

  const state = useAsync(() => api.holdings(auth), [auth]);
  const holdings = useMemo(() => state.data ?? [], [state.data]);

  const [stage, setStage] = useState("OPEN");

  const counts = useMemo(
    () => [
      { id: "OPEN", label: "Outstanding", count: holdings.filter(open).length },
      { id: "PAID", label: "Paid", count: holdings.filter((h) => !open(h)).length },
    ],
    [holdings],
  );

  const shown = holdings.filter((holding) => (stage === "OPEN" ? open(holding) : !open(holding)));

  const face = sum(holdings.filter(open).map((holding) => holding.notional));
  const paid = sum(holdings.filter(open).map((holding) => holding.price));
  const received = sum(holdings.map((holding) => holding.received ?? "0"));

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Portfolio</h1>
          <p className="lede">
            {isInvestor
              ? "Every receivable you were allocated, what you paid for it, and what the debtor has since paid you."
              : "Positions bought in the venue. An issuer sells receivables rather than buying them, so this stays empty."}
          </p>
        </div>

        <div className="stats">
          <Stat label="Face outstanding" value={compact(face)} mark="≡" />
          <Stat label="Paid for it" value={compact(paid)} mark="◈" />
          <Stat label="Received" value={compact(received)} mark="✓" />
        </div>
      </div>

      <div className="toolbar">
        <Chips choices={counts} current={stage} onChoose={setStage} />
      </div>

      <Loader state={state} empty="Nothing bought yet.">
        {() =>
          shown.length === 0 ? (
            <div className="listing">
              <p className="empty">
                {stage === "OPEN"
                  ? "No open positions. A receivable appears here once a batch you won is settled."
                  : "Nothing has been paid back yet."}
              </p>
            </div>
          ) : (
            <div className="listing">
              {shown.map((holding) => (
                <div className="row" key={holding.settlement_id}>
                  <span className="subject">
                    <Discs initial={holding.debtor_ref.charAt(0)} />
                    <span>
                      <span className="title">{holding.number}</span>
                      <span className="tags">
                        <span className="tag">{holding.debtor_ref}</span>
                        {open(holding) ? (
                          <span className="tag">due {relative(holding.due_at)}</span>
                        ) : (
                          <span className="tag">paid {date(holding.received_at ?? "")}</span>
                        )}
                        {holding.settled ? null : (
                          <span className="tag">transfer {holding.state.toLowerCase()}</span>
                        )}
                      </span>
                    </span>
                  </span>

                  <span className="cell-end">{money(holding.notional, holding.currency)}</span>
                  <span className="cell-end">{money(holding.price, holding.currency)}</span>
                  <span className="cell-end">
                    {holding.received ? money(holding.received, holding.currency) : "—"}
                  </span>

                  <span className="cell-end">
                    {holding.is_shortfall ? (
                      <span className="pill is-bad">Paid short</span>
                    ) : (
                      <Status value={holding.status} />
                    )}
                  </span>

                  <span className="cell-actions">
                    <button
                      className="button"
                      onClick={() => navigate(`/listings/${holding.invoice_id}`)}
                    >
                      The receivable
                    </button>
                  </span>
                </div>
              ))}
            </div>
          )
        }
      </Loader>
    </>
  );
}

/** open reports whether the debtor still owes this position. */
function open(holding: Holding): boolean {
  return holding.received === undefined;
}
