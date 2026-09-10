import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";

import { App } from "../App";
import { SessionProvider } from "../session";
import { About } from "./About";

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    // A visitor to this page has no session, and the page must not need one. If anything
    // here reached the API the rejection would be this promise, and the test would say so.
    api: { me: vi.fn().mockRejectedValue(new Error("the public page called the API")) },
  };
});

function at(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <SessionProvider>
        <App />
      </SessionProvider>
    </MemoryRouter>,
  );
}

/*
 * TestTheOnlyPublicRoute, in effect. Everything else in this application is behind a wallet,
 * so a first-time visitor meets a sign-in screen; this page is what makes arriving at the
 * address mean anything, and it is worth nothing if it renders only for people who are
 * already inside.
 */
describe("the public page", () => {
  it("is what the address opens on, and needs no session", async () => {
    at("/");

    expect(await screen.findByRole("heading", { level: 1 })).toBeInTheDocument();
    expect(screen.queryByText(/Sign in with your wallet/)).not.toBeInTheDocument();
  });

  // It was published at /about first, and a link that has been sent to somebody outlives
  // the decision to move it.
  it("still answers at the address it was published under", async () => {
    at("/about");

    expect(await screen.findByRole("heading", { level: 1 })).toBeInTheDocument();
  });

  it("keeps the venue behind the gate", () => {
    at("/portfolio");

    expect(screen.queryByRole("heading", { name: /Invoices, priced without being read/ })).toBeNull();
  });

  // The way in from the page has to reach the gate rather than the page it is on, which is
  // what a link back to the root would have done once the root became this.
  it("offers a door that is not the one the visitor is standing in", () => {
    render(
      <MemoryRouter>
        <About />
      </MemoryRouter>,
    );

    for (const link of screen.getAllByRole("link", { name: /Enter the venue/ })) {
      expect(link).toHaveAttribute("href", "/marketplace");
    }
  });

  /*
   * The disclaimer is the reason this page can make the rest of its claims. A prototype that
   * describes live transactions without saying they carry no money is misleading whether or
   * not it meant to be, so its absence is a failing test rather than a review comment.
   */
  it("says what it is before it says what it does", () => {
    render(
      <MemoryRouter>
        <About />
      </MemoryRouter>,
    );

    const notice = screen.getByText(/This is a prototype/).closest(".notice");
    expect(notice).not.toBeNull();
    expect(within(notice as HTMLElement).getByText(/Testnet only/)).toBeInTheDocument();
  });

  // The one integration that is not live is named, in the same list as the ones that are.
  it("does not claim the confidential workflow is deployed", () => {
    render(
      <MemoryRouter>
        <About />
      </MemoryRouter>,
    );

    const card = screen.getByRole("heading", { name: "Chainlink CRE" }).closest("article");
    expect(within(card as HTMLElement).getByText("simulated")).toBeInTheDocument();
  });

  it("offers a way in and a way to check the claims", () => {
    render(
      <MemoryRouter>
        <About />
      </MemoryRouter>,
    );

    expect(screen.getAllByRole("link", { name: /Enter the venue/ }).length).toBeGreaterThan(0);
    expect(screen.getByRole("link", { name: /0\.0\.10419676/ })).toHaveAttribute(
      "href",
      "https://hashscan.io/testnet/account/0.0.10419676",
    );
  });
});
