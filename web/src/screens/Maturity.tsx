import { useState } from "react";

import { api, ApiError } from "../api/client";
import type { Invoice } from "../api/types";
import { Failure, Fact, Headline, Panel, useAsync } from "../components";
import { date, dateTime, money } from "../format";
import { useSession } from "../session";

/**
 * What happened when the receivable came due.
 *
 * The panel answers one question and does not hedge it: the debtor paid, paid short, or has
 * not paid. When a payment is on record it shows the division as well as the total, because
 * the number a holder cares about is its own share and reading it off a percentage is how
 * disputes start.
 */
export function Maturity({ invoice }: { invoice: Invoice }) {
  const { auth, actor, isOperator, nameOf } = useSession();

  const repayment = useAsync(
    () => api.repayment(auth, invoice.id).catch(missing),
    [auth, invoice.id],
  );

  if (repayment.error) {
    return <Failure error={repayment.error} />;
  }

  const paid = repayment.data;
  if (!paid) {
    return (
      <Panel title="At maturity">
        <p className="muted" style={{ marginTop: 0 }}>
          {invoice.status === "SETTLED"
            ? `Nothing has been received yet. The debtor owes ${money(
                invoice.face,
                invoice.currency,
              )} on ${date(invoice.due_at)}.`
            : "This receivable has not been sold and settled, so nothing is due back yet."}
        </p>
        {isOperator && invoice.status === "SETTLED" ? (
          <RecordPayment invoice={invoice} onRecorded={repayment.reload} />
        ) : null}
      </Panel>
    );
  }

  const mine = paid.shares.find((share) => share.party_id === actor.id);

  return (
    <Panel
      title="At maturity"
      aside={
        <span className={`pill ${paid.is_shortfall ? "is-bad" : "is-done"}`}>
          {paid.is_shortfall ? "Paid short" : "Paid in full"}
        </span>
      }
    >
      <dl className="facts">
        <Headline label="Received">{money(paid.amount, paid.currency)}</Headline>
        <Fact label="Owed">{money(paid.face, paid.currency)}</Fact>
        {paid.is_shortfall ? (
          <Fact label="Shortfall">{money(paid.shortfall, paid.currency)}</Fact>
        ) : null}
        <Fact label="Received on">{dateTime(paid.received_at)}</Fact>
        <Fact label="Reference">{paid.reference}</Fact>
      </dl>

      <table>
        <thead>
          <tr>
            <th>Holder</th>
            <th className="num">Held</th>
            <th className="num">Paid</th>
          </tr>
        </thead>
        <tbody>
          {paid.shares.map((share) => (
            <tr key={share.party_id}>
              <td>
                {nameOf(share.party_id)}
                {share.party_id === actor.id ? <span className="tag">you</span> : null}
              </td>
              <td className="num">{money(share.notional, paid.currency)}</td>
              <td className="num">{money(share.amount, paid.currency)}</td>
            </tr>
          ))}
        </tbody>
      </table>

      {mine ? (
        <p className="muted small" style={{ marginBottom: 0 }}>
          Your share is {money(mine.amount, paid.currency)} against{" "}
          {money(mine.notional, paid.currency)} held.
        </p>
      ) : null}
    </Panel>
  );
}

/**
 * Recording what the debtor paid.
 *
 * Only an operator sees this, because the debtor pays the platform: an issuer that could
 * declare the money arrived would be deciding when its own obligation ended.
 */
function RecordPayment({
  invoice,
  onRecorded,
}: {
  invoice: Invoice;
  onRecorded: () => void;
}) {
  const { auth } = useSession();

  const [amount, setAmount] = useState(invoice.face);
  const [reference, setReference] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  async function act(what: () => Promise<unknown>) {
    setBusy(true);
    setError(null);
    try {
      await what();
      onRecorded();
    } catch (cause) {
      setError(cause);
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <Failure error={error} />

      <div className="form-row">
        <div className="field">
          <label htmlFor="paid">Amount received</label>
          <input
            id="paid"
            value={amount}
            inputMode="decimal"
            onChange={(event) => setAmount(event.target.value)}
          />
        </div>
        <div className="field">
          <label htmlFor="reference">Payment reference</label>
          <input
            id="reference"
            value={reference}
            placeholder="SWIFT-2026-11-07-0042"
            onChange={(event) => setReference(event.target.value)}
          />
        </div>
      </div>

      <div className="actions">
        <button
          className="button is-primary"
          disabled={busy}
          onClick={() =>
            act(() =>
              api.recordRepayment(auth, invoice.id, {
                amount,
                currency: invoice.currency,
                reference,
              }),
            )
          }
        >
          Record the payment
        </button>
        <span className="muted small">
          Less than {money(invoice.face, invoice.currency)} closes this receivable as
          defaulted, and is still divided among its holders.
        </span>
      </div>

      <div className="form-row" style={{ marginTop: 16 }}>
        <div className="field">
          <label htmlFor="default-reason">Nothing arrived — reason</label>
          <input
            id="default-reason"
            value={reason}
            placeholder="the debtor stopped answering"
            onChange={(event) => setReason(event.target.value)}
          />
        </div>
        <div className="actions">
          <button
            className="button"
            disabled={busy || reason.trim() === ""}
            onClick={() => act(() => api.declareDefault(auth, invoice.id, reason))}
          >
            Declare default
          </button>
        </div>
      </div>
    </>
  );
}

/** missing turns "no repayment yet" into an answer, and leaves every other failure alone. */
function missing(cause: unknown): null {
  if (cause instanceof ApiError && cause.status === 404) {
    return null;
  }
  throw cause;
}
