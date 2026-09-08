/**
 * The wire shapes the API returns.
 *
 * Every amount and rate is a string, and stays one. The server keeps money in integer minor
 * units precisely so a price is never rounded; parsing it into a JavaScript number here
 * would undo that at the last step, in the one place a person actually reads it. Strings are
 * formatted for display and never used for arithmetic.
 */

export interface Invoice {
  id: string;
  issuer_id: string;
  debtor_ref: string;
  number: string;
  face: string;
  currency: string;
  issued_at: string;
  due_at: string;
  tenor_days: number;
  status: string;
  failed_from?: string;
  reason?: string;
  assessment_id?: string;
  asset_id?: string;
  version: number;
  created_at: string;
  updated_at: string;
}

export interface Lot {
  id: string;
  invoice_id: string;
  asset_id: string;
  issuer_id: string;
  debtor_ref: string;
  supply: string;
  reserve_price: string;
  currency: string;
  grade: string;
  tenor_days: number;
  implied_yield: string;
}

export interface Auction {
  id: string;
  issuer_id: string;
  status: string;
  opens_at: string;
  closes_at: string;
  lots: Lot[];
  total_supply: string;
  currency: string;
  solver_version?: string;
  certificate_hash?: string;
  reason?: string;
  version: number;
  created_at: string;
  updated_at: string;
}

export interface Bid {
  id: string;
  auction_id: string;
  investor_id: string;
  budget: string;
  currency: string;
  min_yield: string;
  max_grade: string;
  max_tenor_days: number;
  minimum_lot: string;
  max_issuer_share: string;
  max_debtor_share: string;
  status: string;
  version: number;
  created_at: string;
}

export interface Allocation {
  rank: number;
  lot_id: string;
  invoice_id: string;
  asset_id: string;
  bid_id: string;
  investor_id: string;
  notional: string;
  price: string;
  currency: string;
}

export interface Rejection {
  bid_id: string;
  lot_id?: string;
  constraint: string;
}

export interface Solution {
  auction_id: string;
  solver_version: string;
  certificate_hash: string;
  verified: boolean;
  objective: number;
  total_notional: string;
  total_cash: string;
  currency: string;
  allocations: Allocation[];
  rejections: Rejection[];
  branch_nodes: number;
  repair_rounds: number;
  limit_reached: boolean;
}

export interface Contribution {
  feature: string;
  value: string;
  weight: string;
  effect: string;
}

export interface MarketSnapshot {
  payload_hash: string;
  query_hash: string;
  provider: string;
  network: string;
  asset: string;
  benchmark_apr: string;
  liquidity_premium: string;
  volatility: string;
  total_liquidity: string;
  currency: string;
  market_count: number;
  subgraph_ids: string[];
  block_numbers: number[];
  observed_at: string;
  expires_at: string;
  fresh: boolean;
}

export interface Assessment {
  id: string;
  invoice_id: string;
  model_version: string;
  grade: string;
  pd: string;
  lgd: string;
  confidence: string;
  expected_loss: string;
  reserve_price: string;
  platform_fee: string;
  currency: string;
  benchmark_apr: string;
  discount_apr: string;
  premiums: { risk: string; liquidity: string; concentration: string; total: string };
  features: Record<string, string>;
  contributions: Contribution[];
  market_snapshot_hash: string;
  confidential_commitment: string;
  confidential_nonce: string;
  requires_manual_review: boolean;
  created_at: string;
  market_snapshot?: MarketSnapshot;
}

export interface AuditEvent {
  actor: string;
  action: string;
  entity_type: string;
  entity_id: string;
  before_hash?: string;
  after_hash?: string;
  trace_id?: string;
  detail: Record<string, unknown>;
  occurred_at: string;
}

export interface Settlement {
  id: string;
  auction_id: string;
  invoice_id: string;
  asset_id: string;
  investor_id: string;
  from_wallet: string;
  to_wallet: string;
  notional: string;
  price: string;
  currency: string;
  operation_id: string;
  tx_id?: string;
  state: string;
  attempts: number;
  last_error?: string;
  updated_at: string;
}

export interface Organization {
  id: string;
  type: string;
  name: string;
  wallet: string;
  eligibility: string;
  reason?: string;
  can_issue: boolean;
  can_invest: boolean;
  version: number;
  created_at: string;
  updated_at: string;
}

export interface Items<T> {
  items: T[];
}

/** Challenge is the text a wallet is asked to sign, and the nonce that makes it one-shot. */
export interface Challenge {
  nonce: string;
  message: string;
  expires_at: string;
}

export interface Session {
  token: string;
  organization_id: string;
  wallet: string;
  expires_at: string;
}

/** Identity is what the server says about the caller it recognised. */
export interface Identity {
  organization_id: string;
  wallet: string;
  role: string;
  eligible: boolean;
  operator: boolean;
}
