import { useMemo } from "react";
import { useNavigate } from "react-router-dom";

import { api } from "../api/client";
import { Discs, Loader, Stat, useAsync } from "../components";
import { compact, date, money, sum } from "../format";
import { useSession } from "../session";

/**
 * What came back.
 *
 * A venue that only shows what you bought is telling half the story. This is the other
 * half: the receivables that matured, what the debtor actually paid, and — the number that
 * is actually yours — your share of it. The two totals are kept apart on purpose, because a
 * position that returned less than its face is the one thing an investor most needs to see
 * without doing arithmetic.
 */
export function Returns() {
  const { auth, actor } = useSession();
  const navigate = useNavigate();

  const state = useAsync(() => api.repayments(auth), [auth]);
  const repayments = useMemo(() => state.data ?? [], [state.data]);

  const mine = useMemo(
    () =>
      repayments.map((repayment) => ({
        repayment,
        share: repayment.shares.find((s) => s.party_id === actor.id),
      })),
    [repayments, actor.id],
  );

  const received = sum(mine.map((row) => row.share?.amount ?? "0"));
  const held = sum(mine.map((row) => row.share?.notional ?? "0"));
  const short = mine.filter((row) => row.repayment.is_shortfall).length;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Returns</h1>
          <p className="lede">
            Receivables you held that the debtor has since paid. The share shown is yours:
            what arrived is divided in proportion to what each holder held, to the cent.
          </p>
        </div>

        <div className="stats">
          <Stat label="Received" value={compact(received)} mark="◈" />
          <Stat label="Face held" value={compact(held)} mark="≡" />
          <Stat label="Shortfalls" value={String(short)} mark="⚠" />
        </div>
      </div>

      <Loader state={state} empty="Nothing has come back yet.">
        {() =>
          mine.length === 0 ? (
            <div className="listing">
              <p className="empty">
                Nothing has matured yet. A receivable appears here once its debtor pays.
              </p>
            </div>
          ) : (
            <div className="listing">
              {mine.map(({ repayment, share }) => (
                <div className="row" key={repayment.id}>
                  <span className="subject">
                    <Discs initial={repayment.is_shortfall ? "!" : "✓"} />
                    <span>
                      <span className="title">{repayment.reference}</span>
                      <span className="tags">
                        <span className="tag">received {date(repayment.received_at)}</span>
                        {repayment.is_shortfall ? (
                          <span className="tag">
                            short {money(repayment.shortfall, repayment.currency)}
                          </span>
                        ) : null}
                      </span>
                    </span>
                  </span>

                  <span className="cell-end">
                    {share ? money(share.notional, repayment.currency) : "—"}
                  </span>
                  <span className="cell-end">
                    {share ? money(share.amount, repayment.currency) : "—"}
                  </span>
                  <span className="cell-end">
                    <span className={`pill ${repayment.is_shortfall ? "is-bad" : "is-done"}`}>
                      {repayment.is_shortfall ? "Paid short" : "Paid in full"}
                    </span>
                  </span>

                  <span className="cell-actions">
                    <button
                      className="button"
                      onClick={() => navigate(`/listings/${repayment.invoice_id}`)}
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
