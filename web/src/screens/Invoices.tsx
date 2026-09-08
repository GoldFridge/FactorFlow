import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";

import { api } from "../api/client";
import type { Invoice } from "../api/types";
import {
  Chips,
  Discs,
  Expand,
  Failure,
  IconButton,
  Loader,
  Search,
  Stat,
  Status,
  useAsync,
} from "../components";
import { compact, date, money, sum } from "../format";
import { useSession } from "../session";

const stages = [
  { id: "all", label: "All", matches: () => true },
  { id: "preparing", label: "Preparing", matches: (i: Invoice) => preparing.has(i.status) },
  { id: "ready", label: "Ready to list", matches: (i: Invoice) => i.status === "TOKENIZED" },
  { id: "trading", label: "On the market", matches: (i: Invoice) => trading.has(i.status) },
  { id: "financed", label: "Financed", matches: (i: Invoice) => i.status === "SETTLED" },
];

const preparing = new Set(["DRAFT", "UPLOADED", "EXTRACTING", "ASSESSED", "APPROVED", "TOKENIZING"]);
const trading = new Set(["AUCTION_OPEN", "ALLOCATED", "SETTLING"]);

/**
 * The issuer's book, in the same shape as the market: what you hold, where each receivable
 * stands, and the single step the state machine allows next.
 *
 * A button whose only possible outcome is a refusal teaches a user that the screen is
 * guessing, so none is offered where the transition would be rejected.
 */
export function Invoices() {
  const { auth, isIssuer } = useSession();
  const navigate = useNavigate();
  const state = useAsync(() => api.invoices(auth), [auth]);

  const [stage, setStage] = useState("all");
  const [query, setQuery] = useState("");
  const [busy, setBusy] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [selected, setSelected] = useState<string[]>([]);

  const invoices = state.data ?? [];

  const counts = useMemo(
    () =>
      stages.map((s) => ({
        id: s.id,
        label: s.label,
        count: invoices.filter((invoice) => s.matches(invoice)).length,
      })),
    [invoices],
  );

  const shown = useMemo(() => {
    const chosen = stages.find((s) => s.id === stage) ?? stages[0]!;
    const needle = query.trim().toLowerCase();

    return invoices
      .filter((invoice) => chosen.matches(invoice))
      .filter(
        (invoice) =>
          needle === "" ||
          invoice.number.toLowerCase().includes(needle) ||
          invoice.debtor_ref.toLowerCase().includes(needle),
      );
  }, [invoices, stage, query]);

  const bookValue = sum(invoices.map((invoice) => invoice.face));

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
      const auction = await api.openAuction(auth, {
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
          <h1>Your book</h1>
          <p className="lede">
            A receivable is priced before anyone sees it and minted before it is offered. The
            document itself never reaches the platform — only its ciphertext digest does.
          </p>
        </div>

        <div className="stats">
          <Stat label="Book value" value={compact(bookValue)} mark="◈" />
          <Stat label="Receivables" value={String(invoices.length)} mark="≡" />
        </div>
      </div>

      <div className="toolbar">
        <Chips choices={counts} current={stage} onChoose={setStage} />
        <Search value={query} onChange={setQuery} placeholder="Search number or debtor" />
        <div className="toolbar-end">
          {selected.length > 0 ? (
            <button className="button" disabled={busy !== ""} onClick={listBatch}>
              List {selected.length} as a batch
            </button>
          ) : null}
          <button className="button is-primary" onClick={() => navigate("/invoices/new")}>
            Upload a receivable
          </button>
        </div>
      </div>

      <Failure error={error} />

      <Loader state={state} empty="No receivables yet.">
        {() =>
          shown.length === 0 ? (
            <div className="listing">
              <p className="empty">
                {!isIssuer
                  ? "Only an issuer has a book; switch participant to see one."
                  : invoices.length === 0
                    ? "No receivables yet."
                    : "Nothing at that stage."}
              </p>
            </div>
          ) : (
            <div
              className="listing"
              style={{ ["--columns" as string]: "2.2fr 1fr 1fr 1.1fr 190px" }}
            >
              <div className="listing-head">
                <span>Receivable</span>
                <span className="cell-end">Face</span>
                <span className="cell-end">Due</span>
                <span className="cell-end">Status</span>
                <span />
              </div>

              {shown.map((invoice) => (
                <div className="row" key={invoice.id}>
                  <span className="subject">
                    {isIssuer && invoice.status === "TOKENIZED" ? (
                      <input
                        type="checkbox"
                        aria-label={`Include ${invoice.number} in a batch`}
                        checked={selected.includes(invoice.id)}
                        onChange={(event) =>
                          setSelected((current) =>
                            event.target.checked
                              ? [...current, invoice.id]
                              : current.filter((id) => id !== invoice.id),
                          )
                        }
                      />
                    ) : null}
                    <Discs initial={invoice.debtor_ref.charAt(0)} />
                    <span className="naming">
                      <span className="title">{invoice.number}</span>
                      <span className="under">
                        <span className="tag">{invoice.debtor_ref}</span>
                        <span className="tag">{invoice.tenor_days} d</span>
                      </span>
                    </span>
                  </span>

                  <span className="cell-end">{money(invoice.face, invoice.currency)}</span>
                  <span className="cell-end">{date(invoice.due_at)}</span>
                  <span className="cell-end">
                    <Status value={invoice.status} />
                  </span>

                  <span className="cell-actions">
                    <IconButton
                      label="Open receivable"
                      onClick={() => navigate(`/invoices/${invoice.id}`)}
                    >
                      <Expand />
                    </IconButton>
                    <Next invoice={invoice} busy={busy} act={act} enabled={isIssuer} />
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
  const { auth } = useSession();
  if (!enabled) {
    return null;
  }

  if (invoice.status === "ASSESSED") {
    return (
      <button
        className="button is-primary"
        disabled={busy !== ""}
        onClick={() => act(invoice.id, () => api.approveInvoice(auth, invoice.id))}
      >
        Approve price
      </button>
    );
  }

  if (invoice.status === "APPROVED") {
    return (
      <button
        className="button is-primary"
        disabled={busy !== ""}
        onClick={() => act(invoice.id, () => api.tokenizeInvoice(auth, invoice.id))}
      >
        Mint asset
      </button>
    );
  }

  return null;
}
