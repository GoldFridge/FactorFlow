import type {
  Assessment,
  Auction,
  AuditEvent,
  Bid,
  Invoice,
  Items,
  MarketSnapshot,
  Settlement,
  Solution,
} from "./types";

/**
 * ApiError carries what an RFC 9457 problem document said.
 *
 * The trace id is kept because it is the one thing that lets a person looking at a failed
 * screen and a person reading the server logs talk about the same request.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly traceId: string;

  constructor(status: number, message: string, code: string, traceId: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.traceId = traceId;
  }
}

/**
 * request sends one call as the given organization.
 *
 * The caller is named by a header because this build runs in development mode, where the
 * API accepts it in place of a wallet session. It is the only concession the client makes
 * to the demo: every rule behind it is the real one, and the server reads eligibility from
 * the stored organization rather than from anything sent here.
 */
async function request<T>(
  path: string,
  actor: string,
  init: RequestInit = {},
): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (actor) {
    headers.set("X-Demo-Organization", actor);
  }
  if (init.body !== undefined) {
    headers.set("Content-Type", "application/json");
    // A write that is retried must not happen twice. The key is per call rather than per
    // payload, so a resubmitted form is a new intent and a retried fetch is not.
    headers.set("Idempotency-Key", crypto.randomUUID());
  }

  const response = await fetch(path, { ...init, headers });

  if (response.status === 204) {
    return undefined as T;
  }

  const body = await response.text();
  const parsed: unknown = body ? JSON.parse(body) : {};

  if (!response.ok) {
    const problem = parsed as {
      detail?: string;
      title?: string;
      code?: string;
      trace_id?: string;
    };
    throw new ApiError(
      response.status,
      problem.detail || problem.title || `request failed with ${response.status}`,
      problem.code || "unknown",
      problem.trace_id || "",
    );
  }

  return parsed as T;
}

const base = "/api/v1";

export const api = {
  invoices: (actor: string) =>
    request<Items<Invoice>>(`${base}/invoices`, actor).then((r) => r.items),

  invoice: (actor: string, id: string) => request<Invoice>(`${base}/invoices/${id}`, actor),

  assessment: (actor: string, id: string) =>
    request<Assessment>(`${base}/invoices/${id}/assessment`, actor),

  timeline: (actor: string, id: string) =>
    request<Items<AuditEvent>>(`${base}/invoices/${id}/timeline`, actor).then((r) => r.items),

  approveInvoice: (actor: string, id: string) =>
    request<Invoice>(`${base}/invoices/${id}/approve`, actor, { method: "POST", body: "{}" }),

  tokenizeInvoice: (actor: string, id: string) =>
    request<Invoice>(`${base}/invoices/${id}/tokenize`, actor, { method: "POST", body: "{}" }),

  auctions: (actor: string, status?: string) =>
    request<Items<Auction>>(
      `${base}/auctions${status ? `?status=${encodeURIComponent(status)}` : ""}`,
      actor,
    ).then((r) => r.items),

  auction: (actor: string, id: string) => request<Auction>(`${base}/auctions/${id}`, actor),

  bids: (actor: string, id: string) =>
    request<Items<Bid>>(`${base}/auctions/${id}/bids`, actor).then((r) => r.items),

  allocations: (actor: string, id: string) =>
    request<Solution>(`${base}/auctions/${id}/allocations`, actor),

  settlements: (actor: string, id: string) =>
    request<Items<Settlement>>(`${base}/auctions/${id}/settlements`, actor).then((r) => r.items),

  placeBid: (actor: string, auctionID: string, bid: Record<string, unknown>) =>
    request<Bid>(`${base}/auctions/${auctionID}/bids`, actor, {
      method: "POST",
      body: JSON.stringify(bid),
    }),

  openAuction: (actor: string, body: Record<string, unknown>) =>
    request<Auction>(`${base}/auctions`, actor, { method: "POST", body: JSON.stringify(body) }),

  clearAuction: (actor: string, id: string) =>
    request<Solution>(`${base}/auctions/${id}/clear`, actor, { method: "POST", body: "{}" }),

  settleAuction: (actor: string, id: string) =>
    request<Items<Settlement>>(`${base}/auctions/${id}/settle`, actor, {
      method: "POST",
      body: "{}",
    }).then((r) => r.items),

  cancelAuction: (actor: string, id: string, reason: string) =>
    request<Auction>(`${base}/auctions/${id}/cancel`, actor, {
      method: "POST",
      body: JSON.stringify({ reason }),
    }),

  benchmark: (actor: string) => request<MarketSnapshot>(`${base}/market/benchmarks/latest`, actor),
};
