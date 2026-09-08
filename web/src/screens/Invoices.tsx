import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";

import { api } from "../api/client";
import type { Invoice } from "../api/types";
import { Failure, Loader, Panel, Status, useAsync } from "../components";
import { date, money } from "../format";
import { useSession } from "../session";

/**
 * The issuer's book: every receivable and the one thing that can be done to it next.
 *
 * Actions are shown only where the state machine allows them, so the screen never offers a
 * button whose only outcome is a refusal.
 */
export function Invoices() {
  const { actor, isIssuer } = useSession();
  const navigate = useNavigate();
  const state = useAsync(() => api.invoices(actor.id), [actor.id]);

  const [busy, setBusy] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [selected, setSelected] = useState<string[]>([]);

  async function act(id: string, call: () => Promise<unknown>) {
    setBusy(id);
    setError(null);
    try {
      await call();
      state.reload();
    } catch (cause) {
      setError(cause);
    } finally {
      setBusy("");
    }
  }

  async function listBatch() {
    setBusy("batch");
    setError(null);
    try {
      const opens = new Date();
      const closes = new Date(opens.getTime() + 24 * 3600 * 1000);
      const auction = await api.openAuction(actor.id, {
        invoice_ids: selected,
        opens_at: opens.toISOString(),
        closes_at: closes.toISOString(),
      });
      setSelected([]);
      navigate(`/auctions/${auction.id}`);
    } catch (cause) {
      setError(cause);
    } finally {
      setBusy("");
    }
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Receivables</h1>
          <p>
            An invoice is priced before anyone sees it and minted before it is offered. The
            document itself never reaches the platform: only its ciphertext digest does.
          </p>
        </div>
        {isIssuer && selected.length > 0 ? (
          <button className="button is-primary" disabled={busy !== ""} onClick={listBatch}>
            List {selected.length} as a batch
          </button>
        ) : null}
      </div>

      <Failure error={error} />

      <Panel title={isIssuer ? "Your book" : "Receivables"} padded={false}>
        <Loader state={state} empty="No receivables yet.">
          {(invoices) =>
            invoices.length === 0 ? (
              <p className="empty">
                {isIssuer
                  ? "No receivables yet."
                  : "Only an issuer has a book; switch participant to see one."}
              </p>
            ) : (
              <table>
                <thead>
                  <tr>
                    {isIssuer ? <th style={{ width: 34 }} /> : null}
                    <th>Number</th>
                    <th>Debtor</th>
                    <th className="num">Face</th>
                    <th className="num">Due</th>
                    <th>Status</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {invoices.map((invoice) => (
                    <tr key={invoice.id}>
                      {isIssuer ? (
                        <td>
                          {invoice.status === "TOKENIZED" ? (
                            <input
                              type="checkbox"
                              aria-label={`Include ${invoice.number} in a batch`}
                              checked={selected.includes(invoice.id)}
                              onChange={(e) =>
                                setSelected((current) =>
                                  e.target.checked
                                    ? [...current, invoice.id]
                                    : current.filter((id) => id !== invoice.id),
                                )
                              }
                            />
                          ) : null}
                        </td>
                      ) : null}
                      <td>
                        <Link to={`/invoices/${invoice.id}`}>{invoice.number}</Link>
                      </td>
                      <td className="muted">{invoice.debtor_ref}</td>
                      <td className="num">{money(invoice.face, invoice.currency)}</td>
                      <td className="num">{date(invoice.due_at)}</td>
                      <td>
                        <Status value={invoice.status} />
                      </td>
                      <td className="num">
                        <Next invoice={invoice} busy={busy} act={act} enabled={isIssuer} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )
          }
        </Loader>
      </Panel>
    </>
  );
}

/** Next offers the single step the state machine allows from here, and nothing else. */
function Next({
  invoice,
  busy,
  act,
  enabled,
}: {
  invoice: Invoice;
  busy: string;
  act: (id: string, call: () => Promise<unknown>) => Promise<void>;
  enabled: boolean;
}) {
  const { actor } = useSession();
  if (!enabled) {
    return null;
  }

  if (invoice.status === "ASSESSED") {
    return (
      <button
        className="button"
        disabled={busy !== ""}
        onClick={() => act(invoice.id, () => api.approveInvoice(actor.id, invoice.id))}
      >
        Approve price
      </button>
    );
  }

  if (invoice.status === "APPROVED") {
    return (
      <button
        className="button"
        disabled={busy !== ""}
        onClick={() => act(invoice.id, () => api.tokenizeInvoice(actor.id, invoice.id))}
      >
        Mint asset
      </button>
    );
  }

  return null;
}
