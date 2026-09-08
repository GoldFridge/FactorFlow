import { Navigate, useParams } from "react-router-dom";

import { api, ApiError } from "../api/client";
import type { AuditEvent } from "../api/types";
import { Fact, Headline, Loader, Panel, Status, useAsync } from "../components";
import { date, dateTime, money, shortHash, words } from "../format";
import { useSession } from "../session";
import { Price } from "./Price";

/**
 * One receivable, its price, and the reasoning behind that price.
 *
 * This screen is the answer to the only question a participant offered a number really
 * asks: why that number. So it shows the model's own decomposition and the market snapshot
 * the benchmark came from, and then the timeline that proves the sequence actually happened
 * in that order.
 */
export function InvoiceDetail() {
  const { id = "" } = useParams();
  const { auth } = useSession();

  const invoice = useAsync(() => api.invoice(auth, id), [auth, id]);
  const assessment = useAsync(
    () => api.assessment(auth, id).catch(() => null),
    [auth, id],
  );
  const timeline = useAsync(() => api.timeline(auth, id).catch(() => []), [auth, id]);

  // A receivable that is not yours answers "not found", which is the invoice module's rule
  // and the right one. If it was nevertheless offered to the venue there is something you
  // may read, so the screen hands the reader over instead of leaving them at a dead end.
  if (invoice.error instanceof ApiError && invoice.error.status === 404) {
    return <Navigate to={`/listings/${id}`} replace />;
  }

  return (
    <Loader state={invoice}>
      {(inv) => (
        <>
          <div className="page-head">
            <div>
              <h1>{inv.number}</h1>
              <p>
                {inv.debtor_ref} · issued {date(inv.issued_at)} · due {date(inv.due_at)}
              </p>
            </div>
            <Status value={inv.status} />
          </div>

          <Panel title="Receivable">
            <dl className="facts">
              <Headline label="Face value">{money(inv.face, inv.currency)}</Headline>
              <Fact label="Tenor">{inv.tenor_days} days</Fact>
              <Fact label="Asset">{inv.asset_id ? shortHash(inv.asset_id) : "not minted"}</Fact>
              {inv.reason ? <Fact label="Reason">{inv.reason}</Fact> : null}
            </dl>
          </Panel>

          {assessment.data ? <Price assessment={assessment.data} /> : null}

          <Panel title="Provenance" padded={false}>
            <Loader state={timeline} empty="Nothing recorded yet.">
              {(events) =>
                events.length === 0 ? (
                  <p className="empty">Nothing recorded yet.</p>
                ) : (
                  <div className="panel-body">
                    <Timeline events={events} />
                  </div>
                )
              }
            </Loader>
          </Panel>
        </>
      )}
    </Loader>
  );
}

function Timeline({ events }: { events: AuditEvent[] }) {
  const { nameOf } = useSession();

  return (
    <div className="timeline">
      {events.map((event, index) => (
        <div className="event" key={`${event.occurred_at}-${index}`}>
          <span className="when">{dateTime(event.occurred_at)}</span>
          <div>
            <div className="what">{words(event.action)}</div>
            <div className="who">
              {event.actor === "system" ? "the platform" : nameOf(event.actor)}
            </div>
            <div className="detail">
              {Object.entries(event.detail).map(([key, value]) => (
                <span className="chip" key={key}>
                  {key} {String(value)}
                </span>
              ))}
            </div>
          </div>
        </div>
      ))}
    </div>
  );
}
