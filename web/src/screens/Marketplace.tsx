import { useNavigate } from "react-router-dom";

import { api } from "../api/client";
import type { Auction } from "../api/types";
import { Grade, Loader, Panel, Status, useAsync } from "../components";
import { dateTime, money, percent, relative } from "../format";
import { useSession } from "../session";

/**
 * The marketplace is what an investor opens first, so it leads with the two numbers a
 * mandate is written against — the yield a lot implies at its reserve price, and the grade
 * behind that yield — rather than with identifiers.
 */
export function Marketplace() {
  const { actor, nameOf } = useSession();
  const navigate = useNavigate();
  const state = useAsync(() => api.auctions(actor.id), [actor.id]);

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Marketplace</h1>
          <p>
            Every lot is priced by the published model before it is listed. The reserve price
            is the model's, not the issuer's, and the implied yield is what buying at that
            price earns over the remaining tenor.
          </p>
        </div>
      </div>

      <Loader state={state} empty="No batches have been listed yet.">
        {(auctions) =>
          auctions.length === 0 ? (
            <Panel title="Batches">
              <p className="muted">No batches have been listed yet.</p>
            </Panel>
          ) : (
            <>
              {auctions.map((auction) => (
                <Panel
                  key={auction.id}
                  title={`Batch · ${nameOf(auction.issuer_id)}`}
                  padded={false}
                  aside={
                    <span className="small muted">
                      <Status value={auction.status} />{" "}
                      {auction.status === "OPEN"
                        ? `closes ${relative(auction.closes_at)}`
                        : dateTime(auction.updated_at)}
                    </span>
                  }
                >
                  <table>
                    <thead>
                      <tr>
                        <th>Debtor</th>
                        <th>Grade</th>
                        <th className="num">Face</th>
                        <th className="num">Reserve</th>
                        <th className="num">Tenor</th>
                        <th className="num">Implied yield</th>
                      </tr>
                    </thead>
                    <tbody>
                      {auction.lots.map((lot) => (
                        <tr
                          key={lot.id}
                          className="is-linked"
                          onClick={() => navigate(`/auctions/${auction.id}`)}
                        >
                          <td>{lot.debtor_ref}</td>
                          <td>
                            <Grade value={lot.grade} />
                          </td>
                          <td className="num">{money(lot.supply, lot.currency)}</td>
                          <td className="num">{money(lot.reserve_price, lot.currency)}</td>
                          <td className="num">{lot.tenor_days} d</td>
                          <td className="num">{percent(lot.implied_yield)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                  <Footer auction={auction} />
                </Panel>
              ))}
            </>
          )
        }
      </Loader>
    </>
  );
}

function Footer({ auction }: { auction: Auction }) {
  return (
    <div
      className="panel-body small muted"
      style={{ display: "flex", justifyContent: "space-between", gap: 16 }}
    >
      <span>
        {auction.lots.length} lot{auction.lots.length === 1 ? "" : "s"} ·{" "}
        {money(auction.total_supply, auction.currency)} on offer
      </span>
      <a className="mono" href={`/auctions/${auction.id}`}>
        open batch →
      </a>
    </div>
  );
}
