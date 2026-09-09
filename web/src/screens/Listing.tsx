import { Link, useParams } from "react-router-dom";

import { api } from "../api/client";
import { Fact, Grade, Headline, Loader, Panel, Status, useAsync } from "../components";
import { date, money, shortHash } from "../format";
import { useSession } from "../session";
import { Maturity } from "./Maturity";
import { Price } from "./Price";

/**
 * One offered receivable, as a bidder may read it.
 *
 * The issuer's own record answers "not found" to everybody else, and that is the right
 * answer for paper nobody was asked to price. Once it is on the board the question changes:
 * an investor is being asked for money against terms, and terms nobody may read are not a
 * market. So this screen shows what was offered and why it is priced that way, and stops
 * exactly there — the document and the seller's history stay with the seller.
 */
export function Listing() {
  const { id = "" } = useParams();
  const { auth, nameOf } = useSession();

  const listing = useAsync(() => api.listing(auth, id), [auth, id]);

  return (
    <Loader state={listing}>
      {(offer) => (
        <>
          <div className="page-head">
            <div>
              <h1>{offer.number}</h1>
              <p>
                {offer.debtor_ref} · offered by {nameOf(offer.issuer_id)} · issued{" "}
                {date(offer.issued_at)} · due {date(offer.due_at)}
              </p>
            </div>
            <Status value={offer.status} />
          </div>

          <Panel
            title="On offer"
            aside={
              offer.auction_id ? (
                <Link className="small" to={`/auctions/${offer.auction_id}`}>
                  the batch it is in →
                </Link>
              ) : null
            }
          >
            <dl className="facts">
              <Headline label="Face value">{money(offer.face, offer.currency)}</Headline>
              <Fact label="Tenor">{offer.tenor_days} days</Fact>
              <Fact label="Asset">
                {offer.asset_id ? shortHash(offer.asset_id) : "not minted"}
              </Fact>
              {offer.assessment ? (
                <Fact label="Grade">
                  <Grade value={offer.assessment.grade} />
                </Fact>
              ) : null}
              {offer.auction_status ? (
                <Fact label="Batch">{offer.auction_status.toLowerCase()}</Fact>
              ) : null}
            </dl>
            {offer.own ? (
              <p className="muted small" style={{ marginBottom: 0 }}>
                You issued this receivable.{" "}
                <Link to={`/invoices/${offer.invoice_id}`}>Open your own record</Link> for the
                document and the full history.
              </p>
            ) : null}
          </Panel>

          {offer.assessment ? (
            <Price assessment={offer.assessment} />
          ) : (
            <Panel title="Published price">
              <p className="muted" style={{ margin: 0 }}>
                No price is on record for this receivable.
              </p>
            </Panel>
          )}

          {["SETTLED", "MATURED", "DEFAULTED"].includes(offer.status) ? (
            <Maturity
              invoice={{
                id: offer.invoice_id,
                status: offer.status,
                face: offer.face,
                currency: offer.currency,
                due_at: offer.due_at,
              }}
            />
          ) : null}

          <Panel title="What is not here">
            <p className="muted small" style={{ margin: 0 }}>
              The invoice document was encrypted in the issuer's browser and is not readable
              by this platform, let alone by the venue. Its history stays with the issuer
              too. What a bidder is given is the terms above, the score, and the commitment
              that ties that score to the document it was computed from.
            </p>
          </Panel>
        </>
      )}
    </Loader>
  );
}
