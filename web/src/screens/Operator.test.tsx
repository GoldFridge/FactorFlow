import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { api, ApiError, sessionLost } from "../api/client";
import type { Organization } from "../api/types";
import { SessionProvider, participants, useSession } from "../session";
import { Operator } from "./Operator";

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    api: {
      me: vi.fn(),
      organization: vi.fn(),
      organizations: vi.fn(),
      approveOrganization: vi.fn(),
      rejectOrganization: vi.fn(),
      logout: vi.fn(),
    },
  };
});

const operator = participants[4]!;
const issuer = participants[0]!;

function org(overrides: Partial<Organization> = {}): Organization {
  return {
    id: "org-1",
    type: "ISSUER",
    name: "Harbour Freight Ltd",
    wallet: "0x1111111111111111111111111111111111111111",
    eligibility: "PENDING",
    can_issue: false,
    can_invest: false,
    version: 1,
    created_at: "2026-09-08T00:00:00Z",
    updated_at: "2026-09-08T00:00:00Z",
    ...overrides,
  };
}

function signedIn(isOperator: boolean) {
  const as = isOperator ? operator : issuer;
  vi.mocked(api.me).mockResolvedValue({
    organization_id: as.id,
    wallet: "0x00000000000000000000000000000000000000a1",
    role: "owner",
    eligible: true,
    operator: isOperator,
  });
  vi.mocked(api.organization).mockResolvedValue({
    id: as.id,
    type: isOperator ? "OPERATOR" : "ISSUER",
    name: as.name,
    wallet: "0x00000000000000000000000000000000000000a1",
    eligibility: "ELIGIBLE",
    can_issue: !isOperator,
    can_invest: false,
    version: 1,
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
  });
}

function show(isOperator = true) {
  signedIn(isOperator);
  return render(
    <MemoryRouter>
      <SessionProvider>
        <Operator />
      </SessionProvider>
    </MemoryRouter>,
  );
}

describe("the operator console", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
  });

  /*
   * The console exists so that admitting a participant does not require opening a database.
   * A wallet that registered proves it owns an address and nothing else; this is where the
   * venue decides to trust it with more.
   */
  it("admits a wallet that is waiting", async () => {
    vi.mocked(api.organizations).mockResolvedValue([org()]);
    vi.mocked(api.approveOrganization).mockResolvedValue(org({ eligibility: "ELIGIBLE" }));

    show();

    expect(await screen.findByText("Harbour Freight Ltd")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Admit" }));

    await waitFor(() => expect(api.approveOrganization).toHaveBeenCalledWith("", "org-1"));
    // Twice: the first read, then the list read back rather than patched in place.
    expect(api.organizations).toHaveBeenCalledTimes(2);
  });

  /*
   * A refusal is a decision somebody has to live with, so it carries a reason. The button
   * stays out of reach until there is one — an applicant told only "no" cannot fix
   * anything.
   */
  it("will not refuse anyone without a reason", async () => {
    vi.mocked(api.organizations).mockResolvedValue([org()]);
    vi.mocked(api.rejectOrganization).mockResolvedValue(org({ eligibility: "REJECTED" }));

    show();

    const refuse = await screen.findByRole("button", { name: "Refuse" });
    expect(refuse).toBeDisabled();

    await userEvent.type(
      screen.getByLabelText("Reason for refusing Harbour Freight Ltd"),
      "the registered company number does not exist",
    );
    await userEvent.click(refuse);

    await waitFor(() =>
      expect(api.rejectOrganization).toHaveBeenCalledWith(
        "",
        "org-1",
        "the registered company number does not exist",
      ),
    );
  });

  /** The waiting list is what an operator opens the console for, so it opens on it. */
  it("opens on who is waiting, and can show the rest", async () => {
    vi.mocked(api.organizations).mockResolvedValue([
      org(),
      org({ id: "org-2", name: "Alpine Treasury AG", eligibility: "ELIGIBLE" }),
      org({
        id: "org-3",
        name: "Ghost Holdings",
        eligibility: "REJECTED",
        reason: "the wallet is on a sanctions list",
      }),
    ]);

    show();

    expect(await screen.findByText("Harbour Freight Ltd")).toBeInTheDocument();
    expect(screen.queryByText("Alpine Treasury AG")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("tab", { name: /Rejected/ }));
    expect(await screen.findByText("Ghost Holdings")).toBeInTheDocument();
    expect(screen.getByText("the wallet is on a sanctions list")).toBeInTheDocument();
  });

  /** Nobody else has any business reading who applied to take part. */
  it("is closed to everyone but an operator", async () => {
    show(false);

    expect(await screen.findByText(/belongs to the platform's operator/)).toBeInTheDocument();
    expect(api.organizations).not.toHaveBeenCalled();
  });
});

/** Stage reports what the session provider thinks is happening, for the test below. */
function Stage() {
  const { stage } = useSession();
  return <p>stage: {stage}</p>;
}

describe("a session that ends while the app is open", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
  });

  /*
   * Twelve hours pass, or somebody signs out in another tab. The next request answers 401,
   * and that is not an error to render on a panel — it is a different person at the
   * keyboard, so the app goes back to the sign-in screen.
   */
  it("returns to the sign-in screen instead of showing a refusal", async () => {
    signedIn(true);
    vi.mocked(api.organizations).mockRejectedValue(
      new ApiError(401, "authentication is required", "unauthorized", "trace-1"),
    );

    render(
      <MemoryRouter>
        <SessionProvider>
          <Stage />
          <Operator />
        </SessionProvider>
      </MemoryRouter>,
    );

    expect(await screen.findByText("stage: signed-in")).toBeInTheDocument();

    // The client fires this as it turns a 401 into an error; here the mocked client
    // cannot, so the test stands in for it.
    window.dispatchEvent(new CustomEvent(sessionLost));

    expect(await screen.findByText("stage: anonymous")).toBeInTheDocument();
  });
});
