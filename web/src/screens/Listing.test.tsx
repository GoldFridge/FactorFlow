import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { api, ApiError } from "../api/client";
import type { Assessment, Listing as Offered } from "../api/types";
import { SessionProvider, participants } from "../session";
import { InvoiceDetail } from "./InvoiceDetail";
import { Listing } from "./Listing";

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    api: {
      me: vi.fn(),
      organization: vi.fn(),
      listing: vi.fn(),
      invoice: vi.fn(),
      assessment: vi.fn(),
      timeline: vi.fn(),
    },
  };
});

const investor = participants[2]!;
const issuer = participants[0]!;

const assessment: Assessment = {
  id: "a1",
  invoice_id: "i1",
  model_version: "risk-v1",
  grade: "B",
  pd: "0.031250",
  lgd: "0.400000",
  confidence: "0.930000",
  expected_loss: "125.00",
  reserve_price: "9755.32",
  platform_fee: "45.00",
  currency: "USD",
  benchmark_apr: "0.036325",
  discount_apr: "0.152580",
  premiums: {
    risk: "0.090000",
    liquidity: "0.020000",
    concentration: "0.006255",
    total: "0.116255",
  },
  features: { dso_norm: "0.400000" },
  contributions: [
    { feature: "debtor_risk", value: "0.200000", weight: "1.100000", effect: "0.220000" },
    { feature: "dso_norm", value: "0.400000", weight: "0.400000", effect: "0.160000" },
  ],
  market_snapshot_hash: "b7a1",
  confidential_commitment: "0x1a2b",
  confidential_nonce: "0f1e",
  requires_manual_review: false,
  created_at: "2026-09-07T09:00:00Z",
};

function offered(overrides: Partial<Offered> = {}): Offered {
  return {
    invoice_id: "i1",
    issuer_id: issuer.id,
    number: "INV-2026-0001",
    debtor_ref: "ACME Logistics GmbH",
    face: "10000.00",
    currency: "USD",
    issued_at: "2026-08-01T00:00:00Z",
    due_at: "2026-10-01T00:00:00Z",
    tenor_days: 61,
    status: "TOKENIZED",
    asset_id: "0f9c2b7e-2f0a-4a1d-9c1e-7d3b5a6c8e10",
    auction_id: "auction-1",
    auction_status: "OPEN",
    own: false,
    assessment,
    ...overrides,
  };
}

/** The screen is only ever reached behind a signed-in session; this is that answer. */
function signedIn() {
  vi.mocked(api.me).mockResolvedValue({
    organization_id: investor.id,
    wallet: "0x00000000000000000000000000000000000000a4",
    role: "owner",
    eligible: true,
    operator: false,
  });
  vi.mocked(api.organization).mockResolvedValue({
    id: investor.id,
    type: "INVESTOR",
    name: investor.name,
    wallet: "0x00000000000000000000000000000000000000a4",
    eligibility: "ELIGIBLE",
    can_issue: false,
    can_invest: true,
    version: 1,
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
  });
}

function show(path = "/listings/i1") {
  signedIn();
  return render(
    <MemoryRouter initialEntries={[path]}>
      <SessionProvider>
        <Routes>
          <Route path="/listings/:id" element={<Listing />} />
          <Route path="/invoices/:id" element={<InvoiceDetail />} />
        </Routes>
      </SessionProvider>
    </MemoryRouter>,
  );
}

describe("a listed receivable", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
  });

  /*
   * What a bidder is asked for is money against terms, so the terms and the reasoning
   * behind the price have to be readable. This is the screen's whole reason to exist.
   */
  it("shows the terms on offer and why the price is what it is", async () => {
    vi.mocked(api.listing).mockResolvedValue(offered());

    show();

    expect(await screen.findByRole("heading", { name: "INV-2026-0001" })).toBeInTheDocument();
    expect(screen.getByText(/ACME Logistics GmbH/)).toBeInTheDocument();
    expect(screen.getByText("$10,000.00")).toBeInTheDocument();
    expect(screen.getByText("61 days")).toBeInTheDocument();

    // The price and its decomposition, exactly as the model published them.
    expect(screen.getByText("$9,755.32")).toBeInTheDocument();
    expect(screen.getByText("3.63%")).toBeInTheDocument();
    expect(screen.getByText("15.26%")).toBeInTheDocument();
    expect(screen.getByText("Debtor risk")).toBeInTheDocument();

    expect(screen.getByRole("link", { name: /the batch it is in/ })).toHaveAttribute(
      "href",
      "/auctions/auction-1",
    );
  });

  /*
   * The disclosure stops at the terms. Being asked to price a receivable is not being
   * handed the seller's file, and the screen says so rather than leaving the reader to
   * wonder what it is not showing.
   */
  it("says what stays with the issuer", async () => {
    vi.mocked(api.listing).mockResolvedValue(offered());

    show();

    expect(await screen.findByText(/encrypted in the issuer's browser/)).toBeInTheDocument();
    expect(screen.queryByText(/Provenance/)).not.toBeInTheDocument();
    expect(api.timeline).not.toHaveBeenCalled();
    expect(api.invoice).not.toHaveBeenCalled();
  });

  /** An issuer looking at their own lot is told where the fuller record is. */
  it("points the issuer at their own record", async () => {
    vi.mocked(api.listing).mockResolvedValue(offered({ own: true }));

    show();

    expect(await screen.findByRole("link", { name: /Open your own record/ })).toHaveAttribute(
      "href",
      "/invoices/i1",
    );
  });

  /** Terms without a price are still terms, and must not turn the screen into an error. */
  it("stands without an assessment", async () => {
    vi.mocked(api.listing).mockResolvedValue(offered({ assessment: undefined }));

    show();

    expect(await screen.findByText(/No price is on record/)).toBeInTheDocument();
    expect(screen.getByText("$10,000.00")).toBeInTheDocument();
  });

  /*
   * The reported bug: a link into someone else's receivable answered "not found: invoice
   * …" and stopped there. The refusal is correct — the record is not the reader's — but a
   * listed receivable has a disclosure, and the reader belongs on it.
   */
  it("takes a stranger from the issuer's record to the disclosure", async () => {
    vi.mocked(api.invoice).mockRejectedValue(
      new ApiError(404, "not found: invoice i1", "not_found", "trace-1"),
    );
    vi.mocked(api.assessment).mockRejectedValue(new Error("not found"));
    vi.mocked(api.timeline).mockRejectedValue(new Error("not found"));
    vi.mocked(api.listing).mockResolvedValue(offered());

    show("/invoices/i1");

    expect(await screen.findByRole("heading", { name: "INV-2026-0001" })).toBeInTheDocument();
    expect(api.listing).toHaveBeenCalledWith("", "i1");
  });
});
