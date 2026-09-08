import { api } from "../api/client";
import { Fact, Headline, Loader, Panel, useAsync } from "../components";
import { dateTime, money, percent, relative } from "../format";
import { useSession } from "../session";

/**
 * The market every price is quoted against.
 *
 * The provenance is the point of the screen: which subgraphs, which blocks, and the digest
 * of the exact rows that produced the benchmark. A number nobody can trace back to an
 * observation is not a benchmark, it is an assertion.
 */
export function Market() {
  const { auth } = useSession();
  const state = useAsync(() => api.benchmark(auth), [auth]);

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Market data</h1>
          <p>
            The benchmark is the liquidity-weighted median supply yield of the eligible
            markets, and a price is refused outright once the snapshot behind it expires.
          </p>
        </div>
      </div>

      <Loader state={state} empty="No market data has been observed yet.">
        {(snapshot) => (
          <>
            <Panel
              title="Latest snapshot"
              aside={
                <span className={`pill ${snapshot.fresh ? "is-done" : "is-bad"}`}>
                  {snapshot.fresh ? `expires ${relative(snapshot.expires_at)}` : "expired"}
                </span>
              }
            >
              <dl className="facts">
                <Headline label="Benchmark APR">{percent(snapshot.benchmark_apr)}</Headline>
                <Fact label="Liquidity premium">{percent(snapshot.liquidity_premium)}</Fact>
                <Fact label="Volatility">{percent(snapshot.volatility)}</Fact>
                <Fact label="Eligible liquidity">
                  {money(snapshot.total_liquidity, snapshot.currency)}
                </Fact>
                <Fact label="Markets">{snapshot.market_count}</Fact>
                <Fact label="Observed">{dateTime(snapshot.observed_at)}</Fact>
              </dl>
            </Panel>

            <Panel title="Provenance">
              <dl className="facts">
                <Fact label="Provider">{snapshot.provider}</Fact>
                <Fact label="Network">{snapshot.network}</Fact>
                <Fact label="Asset">{snapshot.asset}</Fact>
                <Fact label="Blocks">{snapshot.block_numbers.join(", ") || "—"}</Fact>
              </dl>
              <p className="hash" style={{ marginTop: 14, marginBottom: 0 }}>
                subgraphs {snapshot.subgraph_ids.join(", ") || "—"}
                <br />
                query {snapshot.query_hash}
                <br />
                payload {snapshot.payload_hash}
              </p>
            </Panel>
          </>
        )}
      </Loader>
    </>
  );
}
