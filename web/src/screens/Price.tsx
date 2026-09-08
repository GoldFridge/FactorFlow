import type { Assessment } from "../api/types";
import { Fact, Grade, Headline, Panel } from "../components";
import { dateTime, money, percent, words } from "../format";

/**
 * Price puts the decomposition next to the number.
 *
 * The premiums add up to the discount over the benchmark, and the contributions are the
 * model's own terms — the same values a judge recomputing the score would produce. Nothing
 * here is narrated by anything: these are the stored numbers.
 */
export function Price({ assessment }: { assessment: Assessment }) {
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
