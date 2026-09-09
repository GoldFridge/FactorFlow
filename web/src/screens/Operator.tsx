import { useMemo, useState } from "react";

import { api } from "../api/client";
import type { Organization } from "../api/types";
import { Chips, Discs, Failure, Loader, Panel, Stat, useAsync } from "../components";
import { date, words } from "../format";
import { useSession } from "../session";

/**
 * The operator's console.
 *
 * A venue where anyone can sign in but nobody can be let in is a venue with no
 * participants, and until this screen existed the only way to approve a wallet that had
 * just registered was to open a database. Everything here is a decision the platform is
 * accountable for, so each one is taken by name and each rejection carries a reason the
 * applicant can be told.
 */
export function Operator() {
  const { auth, isOperator } = useSession();

  // Nobody else may ask, so nobody else does: firing a request the server will refuse only
  // puts a 403 in the log for something the screen already knows.
  const state = useAsync(
    () => (isOperator ? api.organizations(auth) : Promise.resolve([])),
    [auth, isOperator],
  );
  const organizations = useMemo(() => state.data ?? [], [state.data]);

  const [stage, setStage] = useState("PENDING");
  const [busy, setBusy] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [reasons, setReasons] = useState<Record<string, string>>({});

  const counts = useMemo(
    () =>
      ["PENDING", "ELIGIBLE", "REJECTED"].map((id) => ({
        id,
        label: words(id),
        count: organizations.filter((org) => org.eligibility === id).length,
      })),
    [organizations],
  );

  const shown = organizations.filter((org) => org.eligibility === stage);

  async function decide(org: Organization, what: () => Promise<unknown>) {
    setBusy(org.id);
    setError(null);
    try {
      await what();
      state.reload();
    } catch (cause) {
      setError(cause);
    } finally {
      setBusy("");
    }
  }

  if (!isOperator) {
    return (
      <Panel title="Operator">
        <p className="muted" style={{ margin: 0 }}>
          This console belongs to the platform's operator.
        </p>
      </Panel>
    );
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Operator</h1>
          <p className="lede">
            Who has asked to take part, and who was let in. A wallet that registers proves
            it owns its address and nothing else — everything the venue trusts a participant
            with starts here.
          </p>
        </div>

        <div className="stats">
          <Stat label="Waiting" value={String(counts[0]?.count ?? 0)} mark="◷" />
          <Stat label="Admitted" value={String(counts[1]?.count ?? 0)} mark="✓" />
          <Stat label="Refused" value={String(counts[2]?.count ?? 0)} mark="✕" />
        </div>
      </div>

      <div className="toolbar">
        <Chips choices={counts} current={stage} onChoose={setStage} />
      </div>

      <Failure error={error} />

      <Loader state={state} empty="No participants yet.">
        {() =>
          shown.length === 0 ? (
            <div className="listing">
              <p className="empty">
                {stage === "PENDING"
                  ? "Nobody is waiting for a decision."
                  : `No ${words(stage).toLowerCase()} participants.`}
              </p>
            </div>
          ) : (
            <div className="listing">
              {shown.map((org) => (
                <div className="row" key={org.id}>
                  <span className="subject">
                    <Discs initial={org.name.charAt(0) || "?"} />
                    <span>
                      <span className="title">{org.name || "Unnamed organization"}</span>
                      <span className="tags">
                        <span className="tag">{words(org.type)}</span>
                        <span className="tag mono">{abbreviate(org.wallet)}</span>
                        <span className="tag">applied {date(org.created_at)}</span>
                      </span>
                    </span>
                  </span>

                  <span className="cell-end">
                    <span className={`pill ${tone(org.eligibility)}`}>
                      {words(org.eligibility)}
                    </span>
                  </span>

                  {org.eligibility === "PENDING" ? (
                    <span className="cell-actions">
                      <input
                        aria-label={`Reason for refusing ${org.name}`}
                        placeholder="reason, if refusing"
                        value={reasons[org.id] ?? ""}
                        onChange={(event) =>
                          setReasons((current) => ({ ...current, [org.id]: event.target.value }))
                        }
                      />
                      <button
                        className="button"
                        disabled={busy === org.id || (reasons[org.id] ?? "").trim() === ""}
                        onClick={() =>
                          decide(org, () =>
                            api.rejectOrganization(auth, org.id, reasons[org.id] ?? ""),
                          )
                        }
                      >
                        Refuse
                      </button>
                      <button
                        className="button is-primary"
                        disabled={busy === org.id}
                        onClick={() => decide(org, () => api.approveOrganization(auth, org.id))}
                      >
                        Admit
                      </button>
                    </span>
                  ) : (
                    <span className="cell-actions">
                      {org.reason ? <span className="muted small">{org.reason}</span> : null}
                    </span>
                  )}
                </div>
              ))}
            </div>
          )
        }
      </Loader>
    </>
  );
}

/** tone maps an eligibility to the meaning the colour is allowed to carry. */
function tone(eligibility: string): string {
  switch (eligibility) {
    case "ELIGIBLE":
      return "is-done";
    case "REJECTED":
      return "is-bad";
    default:
      return "is-working";
  }
}

function abbreviate(address: string): string {
  return address.length > 12 ? `${address.slice(0, 6)}…${address.slice(-4)}` : address;
}
