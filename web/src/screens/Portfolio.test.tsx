import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "../api/client";
import type { Holding } from "../api/types";
import { SessionProvider, participants } from "../session";
import { Portfolio } from "./Portfolio";

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    api: { me: vi.fn(), organization: vi.fn(), holdings: vi.fn() },
  };
});

const investor = participants[2]!;

function holding(overrides: Partial<Holding> = {}): Holding {
  return {
    settlement_id: "s1",
    invoice_id: "i1",
    auction_id: "a1",
    number: "INV-2026-0004",
    debtor_ref: "ACME Logistics GmbH",
    due_at: "2026-12-24T00:00:00Z",
    status: "SETTLED",
    notional: "6000.00",
    price: "5800.00",
    currency: "USD",
    state: "ACCOUNTED",
    settled: true,
    tx_id: "0.0.1@1",
    settled_at: "2026-09-02T00:00:00Z",
    ...overrides,
  };
}

function show() {
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

  return render(
    <MemoryRouter>
      <SessionProvider>
        <Portfolio />
      </SessionProvider>
    </MemoryRouter>,
  );
}

describe("a portfolio", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
  });

  /*
   * The position is the thing an investor holds, and until this screen existed the venue
   * could show what was bid and what came back but nothing in between. Face and price stay
   * two numbers rather than one return: the reader can do that subtraction and does not
   * have to trust this screen to have done it.
   */
  it("shows what is owned, what it cost, and what is still owed", async () => {
    vi.mocked(api.holdings).mockResolvedValue([holding()]);

    show();

    expect(await screen.findByText("INV-2026-0004")).toBeInTheDocument();
    expect(screen.getByText("$6,000.00")).toBeInTheDocument();
    expect(screen.getByText("$5,800.00")).toBeInTheDocument();
    expect(screen.getByText("$6.00K")).toBeInTheDocument();
    expect(screen.getByText("$5.80K")).toBeInTheDocument();
  });

  /*
   * A transfer that has not completed is money the investor has committed and cannot see
   * anywhere else. Hiding it until it finished would keep the venue silent at exactly the
   * moment silence is worst.
   */
  it("shows a transfer that has not completed, and says so", async () => {
    vi.mocked(api.holdings).mockResolvedValue([
      holding({ settled: false, state: "SUBMITTED" }),
    ]);

    show();

    expect(await screen.findByText("transfer submitted")).toBeInTheDocument();
  });

  /*
   * Once the debtor has paid, the position moves out of what is outstanding and carries the
   * reader's own share of the money — not the whole payment, which would flatter a position
   * that was only part of a receivable.
   */
  it("separates what was paid back from what is still outstanding", async () => {
    vi.mocked(api.holdings).mockResolvedValue([
      holding(),
      holding({
        settlement_id: "s2",
        invoice_id: "i2",
        number: "INV-2026-0009",
        status: "MATURED",
        notional: "4000.00",
        price: "3900.00",
        received: "4000.00",
        received_at: "2026-11-07T09:00:00Z",
      }),
    ]);

    show();

    expect(await screen.findByText("INV-2026-0004")).toBeInTheDocument();
    expect(screen.queryByText("INV-2026-0009")).not.toBeInTheDocument();

    // Only the open position counts as outstanding; the received total is what came back.
    expect(screen.getByText("$6.00K")).toBeInTheDocument();
    expect(screen.getByText("$4.00K")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("tab", { name: /Paid/ }));
    expect(await screen.findByText("INV-2026-0009")).toBeInTheDocument();
    expect(screen.queryByText("INV-2026-0004")).not.toBeInTheDocument();
  });

  /** A receivable the debtor paid short is not one that came good, here either. */
  it("marks a position that was paid short", async () => {
    vi.mocked(api.holdings).mockResolvedValue([
      holding({
        status: "DEFAULTED",
        received: "4500.00",
        is_shortfall: true,
        received_at: "2026-11-07T09:00:00Z",
      }),
    ]);

    show();

    await userEvent.click(await screen.findByRole("tab", { name: /Paid/ }));
    expect(await screen.findByText("Paid short")).toBeInTheDocument();
  });

  it("says nothing was bought rather than showing an empty table", async () => {
    vi.mocked(api.holdings).mockResolvedValue([]);

    show();

    expect(await screen.findByText(/No open positions/)).toBeInTheDocument();
  });
});
