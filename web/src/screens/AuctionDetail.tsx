import { useState } from "react";
import { Link, useParams } from "react-router-dom";

import { api, ApiError } from "../api/client";
import type { Solution } from "../api/types";
import { Fact, Failure, Grade, Headline, Loader, Panel, Status, useAsync } from "../components";
import { dateTime, money, percent, relative, shortHash, words } from "../format";
import { useSession } from "../session";

/**
 * One batch, and everything that can be said about it: what is on offer, who bid, what the
 * solver decided, and whether an independent verifier agreed.
 */
export function AuctionDetail() {
  const { id = "" } = useParams();
  const { actor, auth, isInvestor, isOperator, nameOf } = useSession();

  const auction = useAsync(() => api.auction(auth, id), [auth, id]);
  const bids = useAsync(() => api.bids(auth, id).catch(() => []), [auth, id]);
  const solution = useAsync(
    () => api.allocations(auth, id).catch(() => null),
    [auth, id],
  );
  const settlements = useAsync(
    () => api.settlements(auth, id).catch(() => []),
    [auth, id],
  );

  const reloadAll = () => {
    auction.reload();
    bids.reload();
    solution.reload();
    settlements.reload();
  };

  return (
    <Loader state={auction}>
      {(batch) => {
        const mine = batch.issuer_id === actor.id;
        return (
          <>
            <div className="page-head">
              <div>
                <h1>Batch by {nameOf(batch.issuer_id)}</h1>
                <p className="mono faint">{batch.id}</p>
              </div>
              <Status value={batch.status} />
            </div>

            <Panel title="Terms">
              <dl className="facts">
                <Headline label="On offer">
                  {money(batch.total_supply, batch.currency)}
                </Headline>
                <Fact label="Lots">{batch.lots.length}</Fact>
                <Fact label="Opened">{dateTime(batch.opens_at)}</Fact>
                <Fact label="Closes">
                  {dateTime(batch.closes_at)}{" "}
                  <span className="faint small">({relative(batch.closes_at)})</span>
                </Fact>
                {batch.reason ? <Fact label="Reason">{batch.reason}</Fact> : null}
              </dl>
            </Panel>

            <Panel title="Lots" padded={false}>
              <table>
                <thead>
                  <tr>
                    <th>Debtor</th>
                    <th>Grade</th>
                    <th className="num">Face</th>
                    <th className="num">Reserve</th>
                    <th className="num">Tenor</th>
                    <th className="num">Implied yield</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {batch.lots.map((lot) => (
                    <tr key={lot.id}>
                      <td>{lot.debtor_ref}</td>
                      <td>
                        <Grade value={lot.grade} />
                      </td>
                      <td className="num">{money(lot.supply, lot.currency)}</td>
                      <td className="num">{money(lot.reserve_price, lot.currency)}</td>
                      <td className="num">{lot.tenor_days} d</td>
                      <td className="num">{percent(lot.implied_yield)}</td>
                      <td className="num">
                        <Link className="small" to={`/listings/${lot.invoice_id}`}>
                          why this price →
                        </Link>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </Panel>

            {isInvestor && batch.status === "OPEN" && batch.issuer_id !== actor.id ? (
              <BidForm auctionID={batch.id} currency={batch.currency} onPlaced={reloadAll} />
            ) : null}

            {mine || isOperator ? (
              <IssuerActions
                auctionID={batch.id}
                status={batch.status}
                closesAt={batch.closes_at}
                onDone={reloadAll}
              />
            ) : null}

            <Panel title="Bids" padded={false}>
              <Loader state={bids} empty="No bids yet.">
                {(placed) =>
                  placed.length === 0 ? (
                    <p className="empty">No bids yet.</p>
                  ) : (
                    <table>
                      <thead>
                        <tr>
                          <th>Investor</th>
                          <th className="num">Budget</th>
                          <th className="num">Min yield</th>
                          <th>Max grade</th>
                          <th className="num">Max tenor</th>
                          <th>Status</th>
                        </tr>
                      </thead>
                      <tbody>
                        {placed.map((bid) => (
                          <tr key={bid.id}>
                            <td>{nameOf(bid.investor_id)}</td>
                            <td className="num">{money(bid.budget, bid.currency)}</td>
                            <td className="num">{percent(bid.min_yield)}</td>
                            <td>
                              <Grade value={bid.max_grade} />
                            </td>
                            <td className="num">{bid.max_tenor_days} d</td>
                            <td>
                              <Status value={bid.status} />
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  )
                }
              </Loader>
            </Panel>

            {solution.data ? <Clearing solution={solution.data} /> : null}

            {settlements.data && settlements.data.length > 0 ? (
              <Panel title="Settlement" padded={false}>
                <table>
                  <thead>
                    <tr>
                      <th>Investor</th>
                      <th className="num">Notional</th>
                      <th className="num">Paid</th>
                      <th>State</th>
                      <th>Transaction</th>
                    </tr>
                  </thead>
                  <tbody>
                    {settlements.data.map((transfer) => (
                      <tr key={transfer.id}>
                        <td>{nameOf(transfer.investor_id)}</td>
                        <td className="num">{money(transfer.notional, transfer.currency)}</td>
                        <td className="num">{money(transfer.price, transfer.currency)}</td>
                        <td>
                          <Status value={transfer.state} />
                        </td>
                        <td className="mono faint">{transfer.tx_id || "—"}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </Panel>
            ) : null}
          </>
        );
      }}
    </Loader>
  );
}

/**
 * Clearing shows the certificate before the allocations.
 *
 * That ordering is the argument: an allocation anyone can dispute is worth less than one an
 * outsider can recompute, so the hash and the verifier's verdict come first and the rows
 * they cover come after.
 */
function Clearing({ solution }: { solution: Solution }) {
  const { nameOf } = useSession();

  return (
    <>
      <Panel
        title="Clearing certificate"
        aside={
          <span className={`pill ${solution.verified ? "is-done" : "is-bad"}`}>
            {solution.verified ? "Verified independently" : "Not verified"}
          </span>
        }
      >
        <dl className="facts">
          <Fact label="Solver">{solution.solver_version}</Fact>
          <Fact label="Cash raised">{money(solution.total_cash, solution.currency)}</Fact>
          <Fact label="Notional sold">{money(solution.total_notional, solution.currency)}</Fact>
          <Fact label="Search">
            {solution.branch_nodes} nodes · {solution.repair_rounds} repairs
          </Fact>
        </dl>
        <p className="hash" style={{ marginTop: 14, marginBottom: 0 }}>
          {solution.certificate_hash}
        </p>
      </Panel>

      <Panel title="Allocations" padded={false}>
        {solution.allocations.length === 0 ? (
          <p className="empty">No lot found a buyer within its mandate.</p>
        ) : (
          <table>
            <thead>
              <tr>
                <th className="num">#</th>
                <th>Investor</th>
                <th className="num">Notional</th>
                <th className="num">Price</th>
              </tr>
            </thead>
            <tbody>
              {solution.allocations.map((allocation) => (
                <tr key={`${allocation.lot_id}-${allocation.bid_id}`}>
                  <td className="num">{allocation.rank}</td>
                  <td>{nameOf(allocation.investor_id)}</td>
                  <td className="num">{money(allocation.notional, allocation.currency)}</td>
                  <td className="num">{money(allocation.price, allocation.currency)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>

      {solution.rejections.length > 0 ? (
        <Panel title="Why the rest did not fill" padded={false}>
          <table>
            <thead>
              <tr>
                <th>Bid</th>
                <th>Refused by</th>
              </tr>
            </thead>
            <tbody>
              {solution.rejections.map((rejection, index) => (
                <tr key={`${rejection.bid_id}-${index}`}>
                  <td className="mono">{shortHash(rejection.bid_id)}</td>
                  <td>{words(rejection.constraint)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Panel>
      ) : null}
    </>
  );
}

/** BidForm is a mandate rather than a price: constraints the solver has to respect. */
function BidForm({
  auctionID,
  currency,
  onPlaced,
}: {
  auctionID: string;
  currency: string;
  onPlaced: () => void;
}) {
  const { auth } = useSession();
  const [budget, setBudget] = useState("25000.00");
  const [minYield, setMinYield] = useState("0.10");
  const [maxGrade, setMaxGrade] = useState("C");
  const [maxTenor, setMaxTenor] = useState("120");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [placed, setPlaced] = useState(false);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    setPlaced(false);

    try {
      await api.placeBid(auth, auctionID, {
        budget,
        currency,
        min_yield: minYield,
        max_grade: maxGrade,
        max_tenor_days: Number(maxTenor),
        minimum_lot: "0.00",
      });
      setPlaced(true);
      onPlaced();
    } catch (cause) {
      setError(cause instanceof ApiError ? cause : cause);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Panel title="Place a bid">
      <p className="muted small" style={{ marginTop: 0 }}>
        A bid states what you will accept, never what you will pay for a particular lot. The
        solver allocates against these limits and tells you which one refused a lot.
      </p>

      <Failure error={error} />
      {placed ? <div className="notice is-done">Bid placed.</div> : null}

      <form onSubmit={submit}>
        <div className="form-row">
          <div className="field">
            <label htmlFor="budget">Budget ({currency})</label>
            <input id="budget" value={budget} onChange={(e) => setBudget(e.target.value)} />
          </div>
          <div className="field">
            <label htmlFor="min-yield">Minimum yield</label>
            <input
              id="min-yield"
              value={minYield}
              onChange={(e) => setMinYield(e.target.value)}
            />
          </div>
          <div className="field">
            <label htmlFor="max-grade">Worst grade</label>
            <select id="max-grade" value={maxGrade} onChange={(e) => setMaxGrade(e.target.value)}>
              {["A", "B", "C", "D", "E"].map((grade) => (
                <option key={grade}>{grade}</option>
              ))}
            </select>
          </div>
          <div className="field">
            <label htmlFor="max-tenor">Longest tenor (days)</label>
            <input id="max-tenor" value={maxTenor} onChange={(e) => setMaxTenor(e.target.value)} />
          </div>
        </div>
        <button className="button is-primary" disabled={busy} type="submit">
          {busy ? "Placing…" : "Place bid"}
        </button>
      </form>
    </Panel>
  );
}

/** IssuerActions are the steps only the batch's own issuer may take. */
function IssuerActions({
  auctionID,
  status,
  closesAt,
  onDone,
}: {
  auctionID: string;
  status: string;
  closesAt: string;
  onDone: () => void;
}) {
  const { auth } = useSession();
  const [busy, setBusy] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [done, setDone] = useState("");

  const stillOpen = new Date(closesAt).getTime() > Date.now();

  async function act(name: string, call: () => Promise<unknown>) {
    setBusy(name);
    setError(null);
    setDone("");
    try {
      await call();
      setDone(name);
      onDone();
    } catch (cause) {
      setError(cause);
    } finally {
      setBusy("");
    }
  }

  return (
    <Panel title="Issuer actions">
      <Failure error={error} />
      {done ? <div className="notice is-done">{done} completed.</div> : null}

      <div className="actions">
        <button
          className="button is-primary"
          disabled={busy !== "" || status !== "OPEN" || stillOpen}
          title={stillOpen ? "The bidding window has not closed yet" : undefined}
          onClick={() => act("Clearing", () => api.clearAuction(auth, auctionID))}
        >
          {busy === "Clearing" ? "Clearing…" : "Clear batch"}
        </button>

        <button
          className="button"
          disabled={busy !== "" || status !== "CLEARED"}
          onClick={() => act("Settlement", () => api.settleAuction(auth, auctionID))}
        >
          {busy === "Settlement" ? "Settling…" : "Settle allocations"}
        </button>

        <button
          className="button"
          disabled={busy !== "" || (status !== "OPEN" && status !== "DRAFT")}
          onClick={() =>
            act("Cancellation", () =>
              api.cancelAuction(auth, auctionID, "withdrawn from the demo"),
            )
          }
        >
          Cancel batch
        </button>
      </div>

      {stillOpen && status === "OPEN" ? (
        <p className="small faint" style={{ marginBottom: 0, marginTop: 12 }}>
          Clearing is refused until the window closes {relative(closesAt)} — the same rule a
          live batch is held to, so a seeded one is not cleared early either.
        </p>
      ) : null}
    </Panel>
  );
}
