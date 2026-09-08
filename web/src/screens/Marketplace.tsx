import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";

import { api } from "../api/client";
import type { Auction, Lot } from "../api/types";
import {
  Chips,
  Discs,
  IconButton,
  Loader,
  Metered,
  Pulse,
  Search,
  Stat,
  Status,
  useAsync,
} from "../components";
import { compact, money, percent, relative, sum } from "../format";
import { useSession } from "../session";

/** offered is one lot with the batch it belongs to, which is how a row reads. */
interface Offered {
  lot: Lot;
  auction: Auction;
}

const filters = [
  { id: "all", label: "All", matches: () => true },
  { id: "open", label: "Open", matches: (o: Offered) => o.auction.status === "OPEN" },
  { id: "cleared", label: "Cleared", matches: (o: Offered) => o.auction.status === "CLEARED" },
  { id: "settled", label: "Settled", matches: (o: Offered) => o.auction.status === "SETTLED" },
];

/**
 * The venue.
 *
 * The tradable thing is a lot, not a batch, so the list is one row per lot and the batch is
 * a tag under the name. The two numbers in the header describe the venue as a whole, and
 * the meter under each yield compares that row only against what is on this screen.
 */
export function Marketplace() {
  const { auth, isIssuer, nameOf } = useSession();
  const navigate = useNavigate();
  const state = useAsync(() => api.auctions(auth), [auth]);

  const [filter, setFilter] = useState("all");
  const [query, setQuery] = useState("");

  const offered = useMemo<Offered[]>(
    () => (state.data ?? []).flatMap((auction) => auction.lots.map((lot) => ({ lot, auction }))),
    [state.data],
  );

  const counts = useMemo(
    () =>
      filters.map((f) => ({
        id: f.id,
        label: f.label,
        count: offered.filter((o) => f.matches(o)).length,
      })),
    [offered],
  );

  const shown = useMemo(() => {
    const chosen = filters.find((f) => f.id === filter) ?? filters[0]!;
    const needle = query.trim().toLowerCase();

    return offered
      .filter((o) => chosen.matches(o))
      .filter(
        (o) =>
          needle === "" ||
          o.lot.debtor_ref.toLowerCase().includes(needle) ||
          nameOf(o.auction.issuer_id).toLowerCase().includes(needle) ||
          o.lot.grade.toLowerCase() === needle,
      );
  }, [offered, filter, query, nameOf]);

  // The meter is a share of the largest yield on screen, so it recomputes with the filter.
  const widestYield = shown.reduce((max, o) => Math.max(max, Number(o.lot.implied_yield)), 0);

  const listedValue = sum(
    offered.filter((o) => o.auction.status === "OPEN").map((o) => o.lot.supply),
  );
  const openBatches = new Set(
    offered.filter((o) => o.auction.status === "OPEN").map((o) => o.auction.id),
  ).size;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Receivables market</h1>
          <p className="lede">
            Every lot is priced by the published model before it is listed. The reserve is the
            model's, not the issuer's, and the yield is what buying at that price earns over
            the tenor that is left.
          </p>
        </div>

        <div className="stats">
          <Stat label="Listed value" value={compact(listedValue)} mark="◈" />
          <Stat label="Open batches" value={String(openBatches)} mark="◫" />
        </div>
      </div>

      <div className="toolbar">
        <Chips choices={counts} current={filter} onChoose={setFilter} />
        <Search value={query} onChange={setQuery} placeholder="Search debtor, issuer or grade" />
        {isIssuer ? (
          <div className="toolbar-end">
            <button className="button is-quiet" onClick={() => navigate("/invoices")}>
              List a batch
            </button>
          </div>
        ) : null}
      </div>

      <Loader state={state} empty="Nothing has been listed yet.">
        {() =>
          shown.length === 0 ? (
            <div className="listing">
              <p className="empty">
                {offered.length === 0
                  ? "Nothing has been listed yet."
                  : "No lot matches that filter."}
              </p>
            </div>
          ) : (
            <div className="listing">
              <div className="listing-head">
                <span>Lot</span>
                <span className="cell-end">Face</span>
                <span className="cell-end">Reserve</span>
                <span className="cell-end">Tenor</span>
                <span className="cell-end">Implied yield</span>
                <span />
              </div>

              {shown.map(({ lot, auction }) => (
                <div className="row" key={lot.id}>
                  <span className="subject">
                    <Discs initial={lot.debtor_ref.charAt(0)} grade={lot.grade} />
                    <span className="naming">
                      <span className="title">{lot.debtor_ref}</span>
                      <span className="under">
                        <span className="tag">{nameOf(auction.issuer_id)}</span>
                        <Status value={auction.status} />
                        {auction.status === "OPEN" ? (
                          <span className="tag">closes {relative(auction.closes_at)}</span>
                        ) : null}
                      </span>
                    </span>
                  </span>

                  <span className="cell-end">{money(lot.supply, lot.currency)}</span>
                  <span className="cell-end">{money(lot.reserve_price, lot.currency)}</span>
                  <span className="cell-end">{lot.tenor_days} d</span>

                  <span className="cell-end">
                    <Metered
                      value={percent(lot.implied_yield)}
                      share={widestYield === 0 ? 0 : Number(lot.implied_yield) / widestYield}
                    />
                  </span>

                  <span className="cell-actions">
                    <IconButton
                      label="Why this price"
                      onClick={() => navigate(`/invoices/${lot.invoice_id}`)}
                    >
                      <Pulse />
                    </IconButton>
                    <button
                      className="button is-primary"
                      onClick={() => navigate(`/auctions/${auction.id}`)}
                    >
                      {auction.status === "OPEN" ? "Bid" : "Open"}
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
