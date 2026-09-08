import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api, ApiError } from "../api/client";
import { App } from "../App";
import { SessionProvider } from "../session";

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    api: {
      me: vi.fn(),
      organization: vi.fn(),
      challenge: vi.fn(),
      verify: vi.fn(),
      register: vi.fn(),
      logout: vi.fn(),
      auctions: vi.fn().mockResolvedValue([]),
      invoices: vi.fn().mockResolvedValue([]),
    },
  };
});

const address = "0x00000000000000000000000000000000000000a1";

/**
 * A wallet that answers the way an injected one does, so the test exercises the real
 * exchange: request an account, sign the text the server chose, hand the signature back.
 * Nothing is stubbed inside the flow itself.
 */
function fakeWallet(overrides: Partial<Record<string, unknown>> = {}) {
  const request = vi.fn(async ({ method, params }: { method: string; params?: unknown[] }) => {
    if (method in overrides) {
      const answer = overrides[method];
      if (answer instanceof Error) {
        throw answer;
      }
      return answer;
    }
    switch (method) {
      case "eth_requestAccounts":
        return [address];
      case "eth_accounts":
        return [];
      case "personal_sign":
        return `0xsigned:${String(params?.[0] ?? "")}`;
      default:
        throw new Error(`unexpected method ${method}`);
    }
  });

  return { request, on: vi.fn(), removeListener: vi.fn() };
}

function signedIn() {
  vi.mocked(api.me).mockResolvedValue({
    organization_id: "org-1",
    wallet: address,
    role: "owner",
    eligible: true,
    operator: false,
  });
  vi.mocked(api.organization).mockResolvedValue({
    id: "org-1",
    type: "INVESTOR",
    name: "Alpine Treasury AG",
    wallet: address,
    eligibility: "ELIGIBLE",
    can_issue: false,
    can_invest: true,
    version: 1,
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
  });
}

function renderApp() {
  return render(
    <MemoryRouter>
      <SessionProvider>
        <App />
      </SessionProvider>
    </MemoryRouter>,
  );
}

describe("signing in with a wallet", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
    vi.mocked(api.me).mockRejectedValue(new ApiError(401, "unauthorized", "unauthorized", ""));
    vi.mocked(api.challenge).mockResolvedValue({
      nonce: "nonce-1",
      message: "FactorFlow sign-in\nnonce-1\nThis authorizes no payment and moves no funds.",
      expires_at: "2026-09-08T12:05:00Z",
    });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  /**
   * The whole point of the exchange: the wallet signs the text the server chose, and the
   * signature is what the session is built from. A test that stubbed the signing would prove
   * nothing about the only property login rests on.
   */
  it("signs the server's challenge and starts a session", async () => {
    const wallet = fakeWallet();
    vi.stubGlobal("ethereum", wallet);
    vi.mocked(api.verify).mockResolvedValue({
      token: "t",
      organization_id: "org-1",
      wallet: address,
      expires_at: "2026-09-09T00:00:00Z",
    });

    renderApp();
    // The gate is on screen because the cookie was refused; from here the session succeeds.
    const connect = await screen.findByRole("button", { name: "Connect wallet" });
    signedIn();
    await userEvent.click(connect);

    await waitFor(() => expect(api.challenge).toHaveBeenCalledWith(address));

    const [, signature] = vi.mocked(api.verify).mock.calls[0]!;
    expect(signature).toContain("This authorizes no payment");
    expect(wallet.request).toHaveBeenCalledWith({
      method: "personal_sign",
      params: [expect.stringContaining("nonce-1"), address],
    });

    // Signed in, so the venue is what is on screen.
    await waitFor(() => expect(screen.getByText("Alpine Treasury AG")).toBeInTheDocument());
  });

  /** Declining a prompt is the prompt working, and must not read as a fault. */
  it("says plainly when the signature is declined", async () => {
    const rejection = Object.assign(new Error("User rejected the request."), { code: 4001 });
    vi.stubGlobal("ethereum", fakeWallet({ personal_sign: rejection }));

    renderApp();
    await userEvent.click(await screen.findByRole("button", { name: "Connect wallet" }));

    expect(await screen.findByText(/declined/i)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(api.verify).not.toHaveBeenCalled();
  });

  /**
   * A wallet nobody has registered is not an error; it is someone who has not registered
   * yet. The refusal comes only after the signature proves who is asking.
   */
  it("offers registration when the wallet has no organization", async () => {
    vi.stubGlobal("ethereum", fakeWallet());
    vi.mocked(api.verify).mockRejectedValueOnce(
      new ApiError(403, "no organization is registered for this wallet", "forbidden", "t1"),
    );

    renderApp();
    await userEvent.click(await screen.findByRole("button", { name: "Connect wallet" }));

    expect(await screen.findByRole("heading", { name: "Register this wallet" })).toBeVisible();
    expect(screen.getByText(address)).toBeInTheDocument();

    // Registering signs a fresh challenge rather than reusing the spent one.
    vi.mocked(api.register).mockResolvedValue({
      id: "org-1",
      type: "INVESTOR",
      name: "Alpine Treasury AG",
      wallet: address,
      eligibility: "ELIGIBLE",
      can_issue: false,
      can_invest: true,
      version: 1,
      created_at: "2026-09-01T00:00:00Z",
      updated_at: "2026-09-01T00:00:00Z",
    });
    vi.mocked(api.verify).mockResolvedValue({
      token: "t",
      organization_id: "org-1",
      wallet: address,
      expires_at: "2026-09-09T00:00:00Z",
    });
    signedIn();

    await userEvent.type(screen.getByLabelText("Organization"), "Alpine Treasury AG");
    await userEvent.selectOptions(screen.getByLabelText("Acting as"), "INVESTOR");
    await userEvent.click(screen.getByRole("button", { name: "Register" }));

    await waitFor(() => expect(api.register).toHaveBeenCalled());
    const [nonce, , type, name] = vi.mocked(api.register).mock.calls[0]!;
    expect(nonce).toBe("nonce-1");
    expect(type).toBe("INVESTOR");
    expect(name).toBe("Alpine Treasury AG");

    await waitFor(() => expect(screen.getByText("Alpine Treasury AG")).toBeInTheDocument());
  });

  /** With no wallet at all there is nothing to sign with, and the demo says so. */
  it("falls back to a seeded participant only when no wallet exists", async () => {
    renderApp();

    expect(await screen.findByText(/No wallet was found/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Connect wallet" })).toBeNull();

    signedIn();
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));

    await waitFor(() =>
      expect(api.me).toHaveBeenCalledWith("00000000-0000-4000-8000-000000000002"),
    );
  });

  /** A session that is already established survives a reload without asking to sign again. */
  it("restores an existing session from the cookie", async () => {
    vi.stubGlobal("ethereum", fakeWallet());
    signedIn();

    renderApp();

    expect(await screen.findByText("Alpine Treasury AG")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Connect wallet" })).toBeNull();
    expect(api.challenge).not.toHaveBeenCalled();
  });
});
