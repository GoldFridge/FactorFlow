import type {
  Assessment,
  Auction,
  AuditEvent,
  Bid,
  Challenge,
  Identity,
  Invoice,
  Items,
  MarketSnapshot,
  Organization,
  Session,
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
 * request sends one call.
 *
 * A signed-in caller is carried by the session cookie the server set, which is HttpOnly and
 * therefore unreadable here — that is the point of it. The `auth` argument is the escape
 * hatch for a browser with no wallet: it names a seeded organization in a header the API
 * only honours in development, and even then the server reads eligibility from the stored
 * record rather than from anything sent here.
 */
async function request<T>(path: string, auth: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (auth) {
    headers.set("X-Demo-Organization", auth);
  }
  if (init.body !== undefined) {
    headers.set("Content-Type", "application/json");
    // A write that is retried must not happen twice. The key is per call rather than per
    // payload, so a resubmitted form is a new intent and a retried fetch is not.
    headers.set("Idempotency-Key", crypto.randomUUID());
  }

  // same-origin is the default, but the session depends on it, so it is stated.
  const response = await fetch(path, { ...init, headers, credentials: "same-origin" });

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
  /**
   * The login exchange. A wallet asks for a challenge, signs the text it is given, and
   * posts the signature back; the server answers with a session cookie. Nothing here ever
   * sees a private key, and the challenge is spent by the request that verifies it.
   */
  challenge: (wallet: string) =>
    request<Challenge>(`${base}/auth/challenge`, "", {
      method: "POST",
      body: JSON.stringify({ wallet }),
    }),

  verify: (nonce: string, signature: string) =>
    request<Session>(`${base}/auth/verify`, "", {
      method: "POST",
      body: JSON.stringify({ nonce, signature }),
    }),

  me: (auth: string) => request<Identity>(`${base}/auth/me`, auth),

  logout: () => request<void>(`${base}/auth/logout`, "", { method: "POST", body: "{}" }),

  /** register creates the organization a proven wallet will act as. */
  register: (nonce: string, signature: string, type: string, name: string) =>
    request<Organization>(`${base}/organizations`, "", {
      method: "POST",
      body: JSON.stringify({ nonce, signature, type, name }),
    }),

  organization: (auth: string, id: string) =>
    request<Organization>(`${base}/organizations/${id}`, auth),

  createInvoice: (auth: string, invoice: Record<string, unknown>) =>
    request<Invoice>(`${base}/invoices`, auth, { method: "POST", body: JSON.stringify(invoice) }),

  /**
   * uploadDocument sends the ciphertext. The key that opens it stays in the browser, which
   * is why this is the only call that can honestly be described as private.
   */
  uploadDocument: (
    auth: string,
    invoiceID: string,
    document: { ciphertext: string; mime: string; key_ref: string },
  ) =>
    request<{ cipher_hash: string; size_bytes: number; status: string }>(
      `${base}/invoices/${invoiceID}/document/content`,
      auth,
      { method: "POST", body: JSON.stringify(document) },
    ),

  requestAssessment: (auth: string, invoiceID: string) =>
    request<Invoice>(`${base}/invoices/${invoiceID}/assess`, auth, {
      method: "POST",
      body: "{}",
    }),

  invoices: (auth: string) =>
    request<Items<Invoice>>(`${base}/invoices`, auth).then((r) => r.items),

  invoice: (auth: string, id: string) => request<Invoice>(`${base}/invoices/${id}`, auth),

  assessment: (auth: string, id: string) =>
    request<Assessment>(`${base}/invoices/${id}/assessment`, auth),

  timeline: (auth: string, id: string) =>
    request<Items<AuditEvent>>(`${base}/invoices/${id}/timeline`, auth).then((r) => r.items),

  approveInvoice: (auth: string, id: string) =>
    request<Invoice>(`${base}/invoices/${id}/approve`, auth, { method: "POST", body: "{}" }),

  tokenizeInvoice: (auth: string, id: string) =>
    request<Invoice>(`${base}/invoices/${id}/tokenize`, auth, { method: "POST", body: "{}" }),

  auctions: (auth: string, status?: string) =>
    request<Items<Auction>>(
      `${base}/auctions${status ? `?status=${encodeURIComponent(status)}` : ""}`,
      auth,
    ).then((r) => r.items),

  auction: (auth: string, id: string) => request<Auction>(`${base}/auctions/${id}`, auth),

  bids: (auth: string, id: string) =>
    request<Items<Bid>>(`${base}/auctions/${id}/bids`, auth).then((r) => r.items),

  allocations: (auth: string, id: string) =>
    request<Solution>(`${base}/auctions/${id}/allocations`, auth),

  settlements: (auth: string, id: string) =>
    request<Items<Settlement>>(`${base}/auctions/${id}/settlements`, auth).then((r) => r.items),

  placeBid: (auth: string, auctionID: string, bid: Record<string, unknown>) =>
    request<Bid>(`${base}/auctions/${auctionID}/bids`, auth, {
      method: "POST",
      body: JSON.stringify(bid),
    }),

  openAuction: (auth: string, body: Record<string, unknown>) =>
    request<Auction>(`${base}/auctions`, auth, { method: "POST", body: JSON.stringify(body) }),

  clearAuction: (auth: string, id: string) =>
    request<Solution>(`${base}/auctions/${id}/clear`, auth, { method: "POST", body: "{}" }),

  settleAuction: (auth: string, id: string) =>
    request<Items<Settlement>>(`${base}/auctions/${id}/settle`, auth, {
      method: "POST",
      body: "{}",
    }).then((r) => r.items),

  cancelAuction: (auth: string, id: string, reason: string) =>
    request<Auction>(`${base}/auctions/${id}/cancel`, auth, {
      method: "POST",
      body: JSON.stringify({ reason }),
    }),

  benchmark: (auth: string) => request<MarketSnapshot>(`${base}/market/benchmarks/latest`, auth),
};
