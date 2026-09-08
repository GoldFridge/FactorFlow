import { useParams } from "react-router-dom";

import { api } from "../api/client";
import type { Assessment, AuditEvent } from "../api/types";
import { Fact, Grade, Headline, Loader, Panel, Status, useAsync } from "../components";
import { date, dateTime, money, percent, shortHash, words } from "../format";
import { useSession } from "../session";

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

/**
 * Price puts the decomposition next to the number.
 *
 * The premiums add up to the discount over the benchmark, and the contributions are the
 * model's own terms — the same values a judge recomputing the score would produce. Nothing
 * here is narrated by anything: these are the stored numbers.
 */
function Price({ assessment }: { assessment: Assessment }) {
  const widest = assessment.contributions.reduce(
    (max, c) => Math.max(max, Math.abs(Number(c.effect))),
    0,
  );

  return (
    <>
      <Panel
        title="Published price"
        aside={
          assessment.requires_manual_review ? (
            <span className="pill is-working">Held for review</span>
          ) : null
        }
      >
        <dl className="facts">
          <Headline label="Reserve price">
            {money(assessment.reserve_price, assessment.currency)}
          </Headline>
          <Fact label="Grade">
            <Grade value={assessment.grade} />
          </Fact>
          <Fact label="Probability of default">{percent(assessment.pd)}</Fact>
          <Fact label="Loss given default">{percent(assessment.lgd)}</Fact>
          <Fact label="Expected loss">
            {money(assessment.expected_loss, assessment.currency)}
          </Fact>
          <Fact label="Platform fee">
            {money(assessment.platform_fee, assessment.currency)}
          </Fact>
          <Fact label="Model">{assessment.model_version}</Fact>
        </dl>
      </Panel>

      <div className="grid-2">
        <Panel title="How the discount is built">
          <dl className="facts">
            <Fact label="Benchmark">{percent(assessment.benchmark_apr)}</Fact>
            <Fact label="Risk premium">{percent(assessment.premiums.risk)}</Fact>
            <Fact label="Liquidity premium">{percent(assessment.premiums.liquidity)}</Fact>
            <Fact label="Concentration">{percent(assessment.premiums.concentration)}</Fact>
            <Headline label="Discount APR">{percent(assessment.discount_apr)}</Headline>
          </dl>
        </Panel>

        <Panel title="What moved the score">
          <div className="bars">
            {assessment.contributions.map((contribution) => {
              const effect = Math.abs(Number(contribution.effect));
              const share = widest === 0 ? 0 : (effect / widest) * 100;
              return (
                <div className="bar" key={contribution.feature}>
                  <span className="small">{words(contribution.feature)}</span>
                  <span className="track">
                    <span className="fill" style={{ width: `${share}%` }} />
                  </span>
                  <span className="num small mono">{contribution.effect}</span>
                </div>
              );
            })}
          </div>
          <p className="small faint" style={{ marginBottom: 0, marginTop: 12 }}>
            Each bar is a feature's weighted term in the model's log-odds, largest first.
          </p>
        </Panel>
      </div>

      {assessment.market_snapshot ? (
        <Panel
          title="Market it was priced against"
          aside={
            <span className={`pill ${assessment.market_snapshot.fresh ? "is-done" : "is-bad"}`}>
              {assessment.market_snapshot.fresh ? "Fresh" : "Expired"}
            </span>
          }
        >
          <dl className="facts">
            <Fact label="Benchmark APR">{percent(assessment.market_snapshot.benchmark_apr)}</Fact>
            <Fact label="Volatility">{percent(assessment.market_snapshot.volatility)}</Fact>
            <Fact label="Eligible liquidity">
              {money(
                assessment.market_snapshot.total_liquidity,
                assessment.market_snapshot.currency,
              )}
            </Fact>
            <Fact label="Markets">{assessment.market_snapshot.market_count}</Fact>
            <Fact label="Observed">{dateTime(assessment.market_snapshot.observed_at)}</Fact>
          </dl>
          <p className="hash" style={{ marginTop: 14, marginBottom: 0 }}>
            snapshot {assessment.market_snapshot.payload_hash}
          </p>
        </Panel>
      ) : null}

      <Panel title="Confidential run">
        <p className="muted small" style={{ marginTop: 0 }}>
          The document was scored inside the confidential workflow. What the platform stores
          is a commitment to that run and the nonce it was computed over — enough for a
          verifier to recompute the commitment, and not enough to reconstruct the document.
        </p>
        <p className="hash" style={{ marginBottom: 0 }}>
          commitment {assessment.confidential_commitment}
          <br />
          nonce {assessment.confidential_nonce}
        </p>
      </Panel>
    </>
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
