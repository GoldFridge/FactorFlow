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
      // The provider asks what this deployment allows before it offers anything; the
      // seeded-participant shortcut exists only where the server honours it.
      config: vi.fn().mockResolvedValue({ demo_auth: true, secure: false }),
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
 * A provider that answers the way an injected wallet does, so the test exercises the real
 * exchange: request an account, sign the text the server chose, hand the signature back.
 */
function fakeProvider(overrides: Record<string, unknown> = {}, account = address) {
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
        return [account];
      case "personal_sign":
        return `0xsigned:${String(params?.[0] ?? "")}`;
      default:
        throw new Error(`unexpected method ${method}`);
    }
  });

  return { request, on: vi.fn(), removeListener: vi.fn() };
}

/**
 * announce makes wallets discoverable the way EIP-6963 does: the page asks, each extension
 * answers with who it is. That is what lets a browser with MetaMask and Binance Wallet
 * installed offer both instead of whichever loaded last.
 */
function announce(wallets: { rdns: string; name: string; provider: unknown }[]) {
  const handler = () => {
    for (const wallet of wallets) {
      window.dispatchEvent(
        new CustomEvent("eip6963:announceProvider", {
          detail: {
            info: { uuid: wallet.rdns, name: wallet.name, icon: "", rdns: wallet.rdns },
            provider: wallet.provider,
          },
        }),
      );
    }
  };

  window.addEventListener("eip6963:requestProvider", handler);
  return () => window.removeEventListener("eip6963:requestProvider", handler);
}

function signedIn(name = "Alpine Treasury AG") {
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
    name,
    wallet: address,
    eligibility: "ELIGIBLE",
    can_issue: false,
    can_invest: true,
    version: 1,
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
  });
}

function session() {
  return {
    token: "t",
    organization_id: "org-1",
    wallet: address,
    expires_at: "2026-09-09T00:00:00Z",
  };
}

// The gate is what a visitor meets inside the venue; the root of the site is the public
// page, so these tests start where a person who wants in would land.
function renderApp() {
  return render(
    <MemoryRouter initialEntries={["/marketplace"]}>
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
    const provider = fakeProvider();
    const stop = announce([{ rdns: "io.metamask", name: "MetaMask", provider }]);
    vi.mocked(api.verify).mockResolvedValue(session());

    renderApp();
    const button = await screen.findByRole("button", { name: /MetaMask/ });
    signedIn();
    await userEvent.click(button);

    await waitFor(() => expect(api.challenge).toHaveBeenCalledWith(address));

    const [, signature] = vi.mocked(api.verify).mock.calls[0]!;
    expect(signature).toContain("This authorizes no payment");
    expect(provider.request).toHaveBeenCalledWith({
      method: "personal_sign",
      params: [expect.stringContaining("nonce-1"), address],
    });

    await waitFor(() => expect(screen.getByText("Alpine Treasury AG")).toBeInTheDocument());
    stop();
  });

  /*
   * Two wallets used to be one wallet: every extension claimed window.ethereum, so whichever
   * loaded last won and the other was unreachable — which is how a person with MetaMask
   * installed ends up permanently connected to something else. Both are offered now, and
   * clicking one signs with that one.
   */
  it("offers every installed wallet and signs with the chosen one", async () => {
    const metamask = fakeProvider();
    const binance = fakeProvider({}, "0x00000000000000000000000000000000000000b2");
    const stop = announce([
      { rdns: "com.binance.wallet", name: "Binance Wallet", provider: binance },
      { rdns: "io.metamask", name: "MetaMask", provider: metamask },
    ]);
    vi.mocked(api.verify).mockResolvedValue(session());

    renderApp();

    expect(await screen.findByRole("button", { name: /Binance Wallet/ })).toBeVisible();
    const chosen = screen.getByRole("button", { name: /MetaMask/ });
    signedIn();
    await userEvent.click(chosen);

    await waitFor(() => expect(api.verify).toHaveBeenCalled());
    expect(metamask.request).toHaveBeenCalled();
    expect(binance.request).not.toHaveBeenCalled();

    // The choice is remembered, so the next signature does not ask again.
    expect(localStorage.getItem("factorflow.wallet")).toBe("io.metamask");
    stop();
  });

  /** A wallet that never announces itself is still usable, under a name it did not give. */
  it("falls back to the injected provider when nothing announces", async () => {
    const provider = fakeProvider();
    vi.stubGlobal("ethereum", provider);
    vi.mocked(api.verify).mockResolvedValue(session());

    renderApp();
    const button = await screen.findByRole("button", { name: /Injected wallet/ });
    signedIn();
    await userEvent.click(button);

    await waitFor(() => expect(api.verify).toHaveBeenCalled());
  });

  /** Declining a prompt is the prompt working, and must not read as a fault. */
  it("says plainly when the signature is declined", async () => {
    const rejection = Object.assign(new Error("User rejected the request."), { code: 4001 });
    const stop = announce([
      {
        rdns: "io.metamask",
        name: "MetaMask",
        provider: fakeProvider({ personal_sign: rejection }),
      },
    ]);

    renderApp();
    await userEvent.click(await screen.findByRole("button", { name: /MetaMask/ }));

    expect(await screen.findByText(/declined/i)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(api.verify).not.toHaveBeenCalled();
    stop();
  });

  /**
   * A wallet nobody has registered is not an error; it is someone who has not registered
   * yet. The refusal comes only after the signature proves who is asking.
   */
  it("offers registration when the wallet has no organization", async () => {
    const stop = announce([{ rdns: "io.metamask", name: "MetaMask", provider: fakeProvider() }]);
    vi.mocked(api.verify).mockRejectedValueOnce(
      new ApiError(403, "no organization is registered for this wallet", "forbidden", "t1"),
    );

    renderApp();
    await userEvent.click(await screen.findByRole("button", { name: /MetaMask/ }));

    expect(await screen.findByRole("heading", { name: "Register this wallet" })).toBeVisible();
    expect(screen.getByText(address)).toBeInTheDocument();

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
    vi.mocked(api.verify).mockResolvedValue(session());
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
    stop();
  });

  /** With no wallet at all there is nothing to sign with, and the demo says so. */
  it("falls back to a seeded participant only when no wallet exists", async () => {
    renderApp();

    expect(await screen.findByText(/No wallet was found/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /MetaMask/ })).toBeNull();

    signedIn("Northwind Trading GmbH");
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));

    await waitFor(() =>
      expect(api.me).toHaveBeenCalledWith("00000000-0000-4000-8000-000000000002"),
    );
  });

  /** A session that is already established survives a reload without asking to sign again. */
  it("restores an existing session from the cookie", async () => {
    const stop = announce([{ rdns: "io.metamask", name: "MetaMask", provider: fakeProvider() }]);
    signedIn();

    renderApp();

    expect(await screen.findByText("Alpine Treasury AG")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /MetaMask/ })).toBeNull();
    expect(api.challenge).not.toHaveBeenCalled();
    stop();
  });
});
