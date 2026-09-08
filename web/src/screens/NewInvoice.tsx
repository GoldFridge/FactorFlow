import { useState } from "react";
import { useNavigate } from "react-router-dom";

import { api } from "../api/client";
import { Failure, Panel } from "../components";
import { bytesOf, digest, rememberKey, seal, toBase64 } from "../crypto";
import { useSession } from "../session";

/** Step is what the screen is doing, so a person can see where a failure happened. */
type Step = "" | "encrypting" | "creating" | "uploading" | "queueing" | "done";

const steps: Record<Exclude<Step, "" | "done">, string> = {
  encrypting: "Encrypting the document in your browser…",
  creating: "Recording the receivable…",
  uploading: "Uploading the ciphertext…",
  queueing: "Queueing the confidential assessment…",
};

/**
 * Uploading a receivable.
 *
 * The order of operations is the argument this screen makes: the document is encrypted before
 * anything is sent, with a key generated in this tab that never leaves it. The platform
 * receives bytes it cannot read and a digest of those bytes, and prices the invoice from what
 * the confidential workflow reports rather than from anything it could open itself.
 */
export function NewInvoice() {
  const { auth, isIssuer } = useSession();
  const navigate = useNavigate();

  const [debtor, setDebtor] = useState("");
  const [number, setNumber] = useState("");
  const [face, setFace] = useState("");
  const [currency, setCurrency] = useState("USD");
  const [issuedAt, setIssuedAt] = useState(today(-1));
  const [dueAt, setDueAt] = useState(today(60));
  const [file, setFile] = useState<File | null>(null);

  const [step, setStep] = useState<Step>("");
  const [error, setError] = useState<unknown>(null);
  const [receipt, setReceipt] = useState<{ hash: string; size: number } | null>(null);

  const busy = step !== "" && step !== "done";

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    if (!file) {
      setError(new Error("Choose the invoice document to upload."));
      return;
    }

    setError(null);
    setReceipt(null);

    try {
      // Encryption comes first, so nothing is created if the browser cannot do it.
      setStep("encrypting");
      const sealed = await seal(await bytesOf(file));
      const hash = await digest(sealed.ciphertext);

      setStep("creating");
      const invoice = await api.createInvoice(auth, {
        debtor_ref: debtor,
        number,
        face,
        currency,
        issued_at: new Date(issuedAt).toISOString(),
        due_at: new Date(dueAt).toISOString(),
      });

      // The key is kept before the ciphertext is sent: a key lost between those two steps
      // would leave a document nobody can ever open.
      rememberKey(invoice.id, sealed.key);

      setStep("uploading");
      const stored = await api.uploadDocument(auth, invoice.id, {
        ciphertext: toBase64(sealed.ciphertext),
        mime: file.type === "application/json" ? "application/json" : "application/pdf",
        key_ref: "browser-local",
      });

      if (stored.cipher_hash !== hash) {
        throw new Error("The server stored a different digest than the browser computed.");
      }

      setStep("queueing");
      await api.requestAssessment(auth, invoice.id);

      setStep("done");
      setReceipt({ hash, size: sealed.ciphertext.length });
      navigate(`/invoices/${invoice.id}`);
    } catch (cause) {
      setError(cause);
      setStep("");
    }
  }

  if (!isIssuer) {
    return (
      <Panel title="Upload a receivable">
        <p className="muted" style={{ margin: 0 }}>
          Only an issuer sells receivables. Sign in as one to upload.
        </p>
      </Panel>
    );
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Upload a receivable</h1>
          <p className="lede">
            The document is encrypted in this browser before anything is sent. The key is
            generated here and never leaves, so the platform stores bytes it cannot read and
            prices the invoice from what the confidential workflow reports about them.
          </p>
        </div>
      </div>

      <Failure error={error} />

      <Panel title="Invoice">
        <form onSubmit={submit}>
          <div className="form-row">
            <div className="field">
              <label htmlFor="number">Invoice number</label>
              <input
                id="number"
                value={number}
                placeholder="INV-2026-0007"
                onChange={(event) => setNumber(event.target.value)}
              />
            </div>
            <div className="field">
              <label htmlFor="debtor">Debtor</label>
              <input
                id="debtor"
                value={debtor}
                placeholder="ACME Logistics GmbH"
                onChange={(event) => setDebtor(event.target.value)}
              />
            </div>
          </div>

          <div className="form-row">
            <div className="field">
              <label htmlFor="face">Face value</label>
              <input
                id="face"
                value={face}
                placeholder="15000.00"
                inputMode="decimal"
                onChange={(event) => setFace(event.target.value)}
              />
            </div>
            <div className="field">
              <label htmlFor="currency">Currency</label>
              <select
                id="currency"
                value={currency}
                onChange={(event) => setCurrency(event.target.value)}
              >
                <option value="USD">USD</option>
                <option value="EUR">EUR</option>
              </select>
            </div>
            <div className="field">
              <label htmlFor="issued">Issued</label>
              <input
                id="issued"
                type="date"
                value={issuedAt}
                onChange={(event) => setIssuedAt(event.target.value)}
              />
            </div>
            <div className="field">
              <label htmlFor="due">Due</label>
              <input
                id="due"
                type="date"
                value={dueAt}
                onChange={(event) => setDueAt(event.target.value)}
              />
            </div>
          </div>

          <div className="field" style={{ marginBottom: 16 }}>
            <label htmlFor="document">Document (PDF)</label>
            <input
              id="document"
              type="file"
              accept="application/pdf,application/json"
              onChange={(event) => setFile(event.target.files?.[0] ?? null)}
            />
          </div>

          <div className="actions">
            <button className="button is-primary" disabled={busy} type="submit">
              {busy ? "Working…" : "Encrypt and upload"}
            </button>
            {busy ? <span className="muted small">{steps[step as keyof typeof steps]}</span> : null}
          </div>
        </form>
      </Panel>

      {receipt ? (
        <Panel title="What was sent">
          <p className="muted small" style={{ marginTop: 0 }}>
            {receipt.size.toLocaleString()} bytes of ciphertext. The digest below is what the
            server computed over what it stored, and it matches what this browser computed
            before sending — which is the whole of what either side can check.
          </p>
          <p className="hash" style={{ marginBottom: 0 }}>
            {receipt.hash}
          </p>
        </Panel>
      ) : null}
    </>
  );
}

/** today returns a date input value offset by a number of days. */
function today(offsetDays: number): string {
  const date = new Date();
  date.setDate(date.getDate() + offsetDays);
  return date.toISOString().slice(0, 10);
}
