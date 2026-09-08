import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api, ApiError } from "./client";

const actor = "00000000-0000-4000-8000-000000000002";

function respond(status: number, body: unknown, ok = status < 400) {
  return {
    ok,
    status,
    text: () => Promise.resolve(JSON.stringify(body)),
  } as Response;
}

describe("the API client", () => {
  const fetchMock = vi.fn();

  beforeEach(() => {
    vi.stubGlobal("fetch", fetchMock);
    vi.stubGlobal("crypto", { randomUUID: () => "fixed-key" });
    fetchMock.mockReset();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("names the caller on every request", async () => {
    fetchMock.mockResolvedValue(respond(200, { items: [] }));

    await api.invoices(actor);

    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/v1/invoices");
    expect(new Headers(init.headers).get("X-Demo-Organization")).toBe(actor);
  });

  /**
   * A retried write must not happen twice. The key is what lets the server return the first
   * response instead of placing a second bid, so its absence is a correctness bug rather
   * than a missing nicety.
   */
  it("sends an idempotency key with a write and none with a read", async () => {
    fetchMock.mockResolvedValue(respond(201, { id: "b1" }));
    await api.placeBid(actor, "a1", { budget: "100.00" });

    const [, write] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(new Headers(write.headers).get("Idempotency-Key")).toBe("fixed-key");
    expect(new Headers(write.headers).get("Content-Type")).toBe("application/json");

    fetchMock.mockResolvedValue(respond(200, { items: [] }));
    await api.invoices(actor);

    const [, read] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(new Headers(read.headers).get("Idempotency-Key")).toBeNull();
  });

  /**
   * The trace id is the only thing connecting a failed screen to the request the server
   * logged, so a rejection has to carry it rather than collapsing to "request failed".
   */
  it("turns a problem document into an error that still says why", async () => {
    fetchMock.mockResolvedValue(
      respond(
        409,
        {
          title: "The request conflicts with the current state",
          detail: "conflict: auction is not accepting bids",
          code: "conflict",
          trace_id: "trace-4711",
        },
        false,
      ),
    );

    const failure = await api.placeBid(actor, "a1", {}).catch((error: unknown) => error);

    expect(failure).toBeInstanceOf(ApiError);
    const problem = failure as ApiError;
    expect(problem.status).toBe(409);
    expect(problem.code).toBe("conflict");
    expect(problem.traceId).toBe("trace-4711");
    expect(problem.message).toBe("conflict: auction is not accepting bids");
  });

  it("falls back to the status when a failure carries no document", async () => {
    fetchMock.mockResolvedValue(respond(502, {}, false));

    const failure = (await api.invoices(actor).catch((error: unknown) => error)) as ApiError;
    expect(failure.message).toContain("502");
    expect(failure.traceId).toBe("");
  });

  it("unwraps a collection so a screen never sees the envelope", async () => {
    fetchMock.mockResolvedValue(respond(200, { items: [{ id: "i1" }, { id: "i2" }] }));

    await expect(api.invoices(actor)).resolves.toHaveLength(2);
  });

  it("passes a status filter through", async () => {
    fetchMock.mockResolvedValue(respond(200, { items: [] }));

    await api.auctions(actor, "OPEN");
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/auctions?status=OPEN");
  });
});
