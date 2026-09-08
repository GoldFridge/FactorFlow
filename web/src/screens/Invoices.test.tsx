import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "../api/client";
import type { Invoice } from "../api/types";
import { SessionProvider, participants } from "../session";
import { Invoices } from "./Invoices";

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    api: {
      me: vi.fn(),
      organization: vi.fn(),
      logout: vi.fn(),
      invoices: vi.fn(),
      approveInvoice: vi.fn(),
      tokenizeInvoice: vi.fn(),
      openAuction: vi.fn(),
    },
  };
});

const issuer = participants[0]!;

function invoice(overrides: Partial<Invoice>): Invoice {
  return {
    id: "i1",
    issuer_id: issuer.id,
    debtor_ref: "ACME Logistics GmbH",
    number: "INV-2026-0001",
    face: "10000.00",
    currency: "USD",
    issued_at: "2026-08-01T00:00:00Z",
    due_at: "2026-10-01T00:00:00Z",
    tenor_days: 61,
    status: "DRAFT",
    version: 1,
    created_at: "2026-08-01T00:00:00Z",
    updated_at: "2026-08-01T00:00:00Z",
    ...overrides,
  };
}

/**
 * The book is rendered behind a signed-in session, because that is the only way it is ever
 * reached: the provider asks the server who the caller is, and these stubs are that answer.
 */
function signedIn() {
  vi.mocked(api.me).mockResolvedValue({
    organization_id: issuer.id,
    wallet: "0x00000000000000000000000000000000000000a1",
    role: "owner",
    eligible: true,
    operator: false,
  });
  vi.mocked(api.organization).mockResolvedValue({
    id: issuer.id,
    type: "ISSUER",
    name: issuer.name,
    wallet: "0x00000000000000000000000000000000000000a1",
    eligibility: "ELIGIBLE",
    can_issue: true,
    can_invest: false,
    version: 1,
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
  });
}

function renderBook() {
  signedIn();
  return render(
    <MemoryRouter>
      <SessionProvider>
        <Invoices />
      </SessionProvider>
    </MemoryRouter>,
  );
}

describe("the issuer's book", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
  });

  /**
   * The screen offers exactly the step the state machine allows next. A button whose only
   * possible outcome is a refusal teaches a user that the app is unreliable, so the rule is
   * checked here rather than left to whoever edits the table next.
   */
  it("offers only the step the state machine allows", async () => {
    vi.mocked(api.invoices).mockResolvedValue([
      invoice({ id: "draft", number: "INV-1", status: "DRAFT" }),
      invoice({ id: "assessed", number: "INV-2", status: "ASSESSED" }),
      invoice({ id: "approved", number: "INV-3", status: "APPROVED" }),
      invoice({ id: "settled", number: "INV-4", status: "SETTLED" }),
    ]);

    renderBook();

    const rowFor = async (number: string) =>
      (await screen.findByText(number)).closest(".row") as HTMLElement;

    // Every row can be opened; what varies is whether a step is offered beside that.
    const stepIn = (row: HTMLElement) =>
      within(row)
        .getAllByRole("button")
        .filter((button) => button.getAttribute("aria-label") !== "Open receivable");

    expect(stepIn(await rowFor("INV-1"))).toHaveLength(0);
    expect(stepIn(await rowFor("INV-2"))[0]).toHaveTextContent("Approve price");
    expect(stepIn(await rowFor("INV-3"))[0]).toHaveTextContent("Mint asset");
    expect(stepIn(await rowFor("INV-4"))).toHaveLength(0);
  });

  it("reloads after an action so the new status is the one on screen", async () => {
    vi.mocked(api.invoices)
      .mockResolvedValueOnce([invoice({ id: "i1", status: "ASSESSED" })])
      .mockResolvedValueOnce([invoice({ id: "i1", status: "APPROVED" })]);
    vi.mocked(api.approveInvoice).mockResolvedValue(invoice({ id: "i1", status: "APPROVED" }));

    renderBook();
    await userEvent.click(await screen.findByRole("button", { name: "Approve price" }));

    await waitFor(() => expect(api.approveInvoice).toHaveBeenCalledWith("", "i1"));
    expect(await screen.findByRole("button", { name: "Mint asset" })).toBeInTheDocument();
  });

  /**
   * A refusal is shown in the server's own words. Swallowing it would leave a screen that
   * simply did nothing, which is the failure mode a demo cannot recover from.
   */
  it("shows why an action was refused", async () => {
    vi.mocked(api.invoices).mockResolvedValue([invoice({ id: "i1", status: "ASSESSED" })]);
    vi.mocked(api.approveInvoice).mockRejectedValue(
      new Error("conflict: invoice i1 is not awaiting approval"),
    );

    renderBook();
    await userEvent.click(await screen.findByRole("button", { name: "Approve price" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("not awaiting approval");
  });

  /** Only a minted receivable can be listed, so only those may be selected for a batch. */
  it("lets a minted receivable be listed and nothing else", async () => {
    vi.mocked(api.invoices).mockResolvedValue([
      invoice({ id: "minted", number: "INV-1", status: "TOKENIZED" }),
      invoice({ id: "draft", number: "INV-2", status: "DRAFT" }),
    ]);
    vi.mocked(api.openAuction).mockResolvedValue({
      id: "a1",
      issuer_id: issuer.id,
      status: "OPEN",
      opens_at: "2026-09-08T00:00:00Z",
      closes_at: "2026-09-09T00:00:00Z",
      lots: [],
      total_supply: "10000.00",
      currency: "USD",
      version: 1,
      created_at: "2026-09-08T00:00:00Z",
      updated_at: "2026-09-08T00:00:00Z",
    });

    renderBook();

    const boxes = await screen.findAllByRole("checkbox");
    expect(boxes).toHaveLength(1);

    await userEvent.click(boxes[0]!);
    await userEvent.click(await screen.findByRole("button", { name: /List 1 as a batch/ }));

    await waitFor(() => expect(api.openAuction).toHaveBeenCalled());
    const [, body] = vi.mocked(api.openAuction).mock.calls[0]!;
    expect(body.invoice_ids).toEqual(["minted"]);
  });
});
