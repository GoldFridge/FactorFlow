import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { api, ApiError } from "../api/client";
import type { Invoice, Repayment } from "../api/types";
import { SessionProvider, participants } from "../session";
import { Maturity } from "./Maturity";
import { Returns } from "./Returns";

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    api: {
      me: vi.fn(),
      organization: vi.fn(),
      repayment: vi.fn(),
      recordRepayment: vi.fn(),
      declareDefault: vi.fn(),
      repayments: vi.fn(),
    },
  };
});

const issuer = participants[0]!;
const investor = participants[2]!;
const operator = participants[4]!;

const invoice: Invoice = {
  id: "i1",
  issuer_id: issuer.id,
  debtor_ref: "ACME Logistics GmbH",
  number: "INV-2026-0007",
  face: "10000.00",
  currency: "USD",
  issued_at: "2026-09-08T00:00:00Z",
  due_at: "2026-11-07T00:00:00Z",
  tenor_days: 60,
  status: "SETTLED",
  version: 9,
  created_at: "2026-09-08T00:00:00Z",
  updated_at: "2026-11-07T00:00:00Z",
};

function repayment(overrides: Partial<Repayment> = {}): Repayment {
  return {
    id: "r1",
    invoice_id: "i1",
    face: "10000.00",
    amount: "10000.00",
    shortfall: "0.00",
    currency: "USD",
    is_shortfall: false,
    reference: "SWIFT-2026-11-07-0042",
    received_at: "2026-11-07T09:00:00Z",
    recorded_by: operator.id,
    shares: [
      { party_id: investor.id, notional: "6000.00", amount: "6000.00" },
      { party_id: issuer.id, notional: "4000.00", amount: "4000.00" },
    ],
    created_at: "2026-11-07T10:00:00Z",
    ...overrides,
  };
}

/** signedIn answers the session provider as one of the seeded participants. */
function signedIn(as: (typeof participants)[number], isOperator = false) {
  vi.mocked(api.me).mockResolvedValue({
    organization_id: as.id,
    wallet: "0x00000000000000000000000000000000000000a1",
    role: "owner",
    eligible: true,
    operator: isOperator,
  });
  vi.mocked(api.organization).mockResolvedValue({
    id: as.id,
    type: as.role,
    name: as.name,
    wallet: "0x00000000000000000000000000000000000000a1",
    eligibility: "ELIGIBLE",
    can_issue: as.role === "ISSUER",
    can_invest: as.role === "INVESTOR",
    version: 1,
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
  });
}

function show(node: React.ReactNode, as = investor, isOperator = false) {
  signedIn(as, isOperator);
  return render(
    <MemoryRouter>
      <SessionProvider>{node}</SessionProvider>
    </MemoryRouter>,
  );
}

describe("maturity", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
    vi.mocked(api.repayments).mockResolvedValue([]);
  });

  /*
   * The number a holder cares about is its own share, and reading it off a percentage is
   * how disputes start. So the division is shown in full, with the reader's own line named.
   */
  it("shows what arrived and how it was divided", async () => {
    vi.mocked(api.repayment).mockResolvedValue(repayment());

    show(<Maturity invoice={invoice} />);

    const received = (await screen.findByText("Received")).closest(".fact") as HTMLElement;
    expect(within(received).getByText("$10,000.00")).toBeInTheDocument();
    expect(screen.getByText("Paid in full")).toBeInTheDocument();

    const row = screen.getByText(investor.name).closest("tr")!;
    // Held and paid, which for a receivable paid in full are the same number.
    expect(within(row).getAllByText("$6,000.00")).toHaveLength(2);
    expect(within(row).getByText("you")).toBeInTheDocument();

    expect(screen.getByText(/Your share is/)).toHaveTextContent("$6,000.00");
  });

  /*
   * A receivable paid short is not one that came good, and the panel says so in the same
   * place it would have said the opposite.
   */
  it("says plainly when the debtor paid short", async () => {
    vi.mocked(api.repayment).mockResolvedValue(
      repayment({
        amount: "7500.00",
        shortfall: "2500.00",
        is_shortfall: true,
        shares: [
          { party_id: investor.id, notional: "6000.00", amount: "4500.00" },
          { party_id: issuer.id, notional: "4000.00", amount: "3000.00" },
        ],
      }),
    );

    show(<Maturity invoice={{ ...invoice, status: "DEFAULTED" }} />);

    expect(await screen.findByText("Paid short")).toBeInTheDocument();
    expect(screen.getByText("$2,500.00")).toBeInTheDocument();
    expect(screen.getByText(/Your share is/)).toHaveTextContent("$4,500.00");
  });

  /** Nothing received yet is an answer, not an error. */
  it("waits for the debtor without looking broken", async () => {
    vi.mocked(api.repayment).mockRejectedValue(
      new ApiError(404, "not found: repayment of invoice i1", "not_found", "trace-1"),
    );

    show(<Maturity invoice={invoice} />);

    expect(await screen.findByText(/Nothing has been received yet/)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Record the payment" })).not.toBeInTheDocument();
  });

  /*
   * Recording a payment is an operator's job, because the debtor pays the platform: an
   * issuer that could declare the money arrived would decide when its own obligation ended.
   */
  it("lets an operator record what the debtor paid", async () => {
    vi.mocked(api.repayment).mockRejectedValue(
      new ApiError(404, "not found", "not_found", "trace-1"),
    );
    vi.mocked(api.recordRepayment).mockResolvedValue(repayment());

    show(<Maturity invoice={invoice} />, operator, true);

    await userEvent.type(
      await screen.findByLabelText("Payment reference"),
      "SWIFT-2026-11-07-0042",
    );
    await userEvent.click(screen.getByRole("button", { name: "Record the payment" }));

    await waitFor(() =>
      expect(api.recordRepayment).toHaveBeenCalledWith("", "i1", {
        amount: "10000.00",
        currency: "USD",
        reference: "SWIFT-2026-11-07-0042",
      }),
    );
  });

  /** A default needs a reason; the button stays out of reach until there is one. */
  it("asks an operator why before declaring a default", async () => {
    vi.mocked(api.repayment).mockRejectedValue(
      new ApiError(404, "not found", "not_found", "trace-1"),
    );
    vi.mocked(api.declareDefault).mockResolvedValue({
      invoice_id: "i1",
      status: "DEFAULTED",
      reason: "the debtor stopped answering",
    });

    show(<Maturity invoice={invoice} />, operator, true);

    const declare = await screen.findByRole("button", { name: "Declare default" });
    expect(declare).toBeDisabled();

    await userEvent.type(
      screen.getByLabelText("Nothing arrived — reason"),
      "the debtor stopped answering",
    );
    await userEvent.click(declare);

    await waitFor(() =>
      expect(api.declareDefault).toHaveBeenCalledWith("", "i1", "the debtor stopped answering"),
    );
  });
});

describe("returns", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
  });

  /*
   * The totals are the reader's own, not the payment's: an investor who held part of a
   * receivable is owed part of what arrived, and a screen that added up the whole payment
   * would flatter every position on it.
   */
  it("totals the reader's own share, not the whole payment", async () => {
    vi.mocked(api.repayments).mockResolvedValue([
      repayment(),
      repayment({
        id: "r2",
        invoice_id: "i2",
        reference: "SWIFT-2026-11-08-0043",
        amount: "7500.00",
        shortfall: "2500.00",
        is_shortfall: true,
        shares: [{ party_id: investor.id, notional: "4000.00", amount: "3000.00" }],
      }),
    ]);

    show(<Returns />);

    expect(await screen.findByText("SWIFT-2026-11-07-0042")).toBeInTheDocument();

    // 6000.00 + 3000.00 of receipts against 6000.00 + 4000.00 of face held.
    expect(screen.getByText("$9.00K")).toBeInTheDocument();
    expect(screen.getByText("$10.00K")).toBeInTheDocument();
    expect(screen.getByText("Paid short")).toBeInTheDocument();
  });

  it("says nothing has matured rather than showing an empty table", async () => {
    vi.mocked(api.repayments).mockResolvedValue([]);

    show(<Returns />);

    expect(await screen.findByText(/Nothing has matured yet/)).toBeInTheDocument();
  });
});
