import { useState } from "react";

import { Failure } from "../components";
import { participants, useSession } from "../session";
import { WalletError, type Wallet } from "../wallet";

/**
 * The way in.
 *
 * A participant here is a wallet, so signing in is proving control of one rather than
 * knowing a password: the server issues a one-shot challenge, the wallet signs it, and the
 * recovered address names the organization. Nothing is typed, nothing is stored, and there
 * is no account to lose.
 */
export function SignIn() {
  const { stage, wallets, connect, busy, error } = useSession();

  if (stage === "unregistered") {
    return <Register />;
  }

  return (
    <div className="gate">
      <div className="gate-card">
        <span className="gate-mark" aria-hidden="true">
          F
        </span>
        <h1>Sign in with your wallet</h1>
        <p className="muted">
          You will be asked to sign a short message. It authorizes no payment and moves no
          funds — the signature only proves the address is yours.
        </p>

        <Refusal error={error} />

        {wallets.length === 0 ? (
          <NoWallet />
        ) : (
          <div className="wallet-list">
            {wallets.map((option) => (
              <button
                key={option.id}
                className="wallet-option"
                disabled={busy}
                onClick={() => void connect(option.id)}
              >
                <Badge wallet={option} />
                <span className="wallet-name">{option.name}</span>
                <span className="wallet-go" aria-hidden="true">
                  →
                </span>
              </button>
            ))}
          </div>
        )}

        {wallets.length > 1 ? (
          <p className="faint small" style={{ marginTop: 14, marginBottom: 0 }}>
            More than one wallet is installed. Each announced itself, so the choice is yours
            rather than whichever loaded last.
          </p>
        ) : null}
      </div>
    </div>
  );
}

/** Badge shows the wallet's own icon, or its initial when it announced none. */
function Badge({ wallet }: { wallet: Wallet }) {
  if (wallet.icon) {
    return <img className="wallet-icon" src={wallet.icon} alt="" />;
  }
  return (
    <span className="wallet-icon is-letter" aria-hidden="true">
      {wallet.name.charAt(0)}
    </span>
  );
}

/**
 * Refusal separates a decision from a fault.
 *
 * Declining a prompt is the prompt working, so it is said plainly and without the alarm an
 * error banner carries.
 */
function Refusal({ error }: { error: unknown }) {
  if (error instanceof WalletError && error.kind === "rejected") {
    return <p className="faint small">The signature was declined. Nothing was sent.</p>;
  }
  return <Failure error={error} />;
}

/**
 * Without an injected wallet there is nothing to sign with, so the demo falls back to naming
 * a seeded participant. The API accepts that header only in development, which is stated
 * here rather than left for someone to discover.
 */
function NoWallet() {
  const { useDemo, busy } = useSession();
  const [chosen, setChosen] = useState(participants[0]!.id);

  return (
    <>
      <p className="faint small">
        No wallet was found in this browser. Install one to sign in properly, or continue as a
        seeded participant — a development shortcut the API refuses outside development.
      </p>

      <div className="gate-fallback">
        <select
          aria-label="Seeded participant"
          value={chosen}
          onChange={(event) => setChosen(event.target.value)}
        >
          {participants.map((participant) => (
            <option key={participant.id} value={participant.id}>
              {participant.name} · {participant.role.toLowerCase()}
            </option>
          ))}
        </select>
        <button className="button" disabled={busy} onClick={() => useDemo(chosen)}>
          Continue
        </button>
      </div>
    </>
  );
}

/** Register is what a proven wallet with no organization behind it sees. */
function Register() {
  const { pendingWallet, register, busy, error, signOut } = useSession();
  const [name, setName] = useState("");
  const [type, setType] = useState("ISSUER");

  return (
    <div className="gate">
      <div className="gate-card">
        <h1>Register this wallet</h1>
        <p className="muted">
          The address below proved itself, but no organization is registered to it yet. You
          will be asked to sign twice: once to create the organization, once to sign in as it.
        </p>
        <p className="hash">{pendingWallet}</p>

        <Refusal error={error} />

        <form
          className="gate-form"
          onSubmit={(event) => {
            event.preventDefault();
            void register(type, name);
          }}
        >
          <div className="field">
            <label htmlFor="org-name">Organization</label>
            <input
              id="org-name"
              value={name}
              placeholder="Northwind Trading GmbH"
              onChange={(event) => setName(event.target.value)}
            />
          </div>

          <div className="field">
            <label htmlFor="org-type">Acting as</label>
            <select id="org-type" value={type} onChange={(event) => setType(event.target.value)}>
              <option value="ISSUER">Issuer — sells its receivables</option>
              <option value="INVESTOR">Investor — buys them</option>
            </select>
          </div>

          <div className="actions">
            <button className="button is-primary" disabled={busy || name.trim() === ""}>
              {busy ? "Waiting for the wallet…" : "Register"}
            </button>
            <button className="button" type="button" disabled={busy} onClick={signOut}>
              Use another wallet
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}
