/**
 * The browser wallets, through EIP-6963 and EIP-1193.
 *
 * Nothing here holds a key or produces a signature: a wallet does both, behind a prompt the
 * person answers. What this module owns is the awkward surface around that.
 *
 * The awkwardness is real. Every injected wallet used to claim `window.ethereum`, so with two
 * installed the last one to load won and the other was unreachable — a person with MetaMask
 * and Binance Wallet could be permanently connected to whichever happened to inject second.
 * EIP-6963 replaced that scramble with an announcement: each wallet says who it is, and the
 * choice belongs to the person rather than to load order.
 */

/** Eip1193 is the interface every injected wallet agrees to expose. */
export interface Eip1193 {
  request(args: { method: string; params?: unknown[] }): Promise<unknown>;
  on?(event: string, handler: (...args: unknown[]) => void): void;
  removeListener?(event: string, handler: (...args: unknown[]) => void): void;
}

/** Announcement is what a wallet publishes about itself under EIP-6963. */
interface Announcement {
  info: { uuid: string; name: string; icon: string; rdns: string };
  provider: Eip1193;
}

/** Wallet is one choice a person can make. */
export interface Wallet {
  /** id is the wallet's reverse-DNS name, stable across sessions and reinstalls. */
  id: string;
  name: string;
  /** icon is a data URI the wallet supplies, or empty for a wallet that announced none. */
  icon: string;
  provider: Eip1193;
}

declare global {
  interface Window {
    ethereum?: Eip1193;
  }
}

/** WalletError is a refusal a screen can put into words. */
export class WalletError extends Error {
  readonly kind: "absent" | "rejected" | "failed";

  constructor(kind: WalletError["kind"], message: string) {
    super(message);
    this.name = "WalletError";
    this.kind = kind;
  }
}

/** legacyID names the unannounced provider, which cannot tell us what it is. */
export const legacyID = "injected";

/**
 * discover subscribes to wallet announcements and reports the list as it grows.
 *
 * Wallets answer the request event synchronously in practice, but nothing in the standard
 * says they must, and an extension that loads late would be missed by a single snapshot. So
 * this stays subscribed and calls back, and the caller re-renders rather than polling.
 *
 * It returns an unsubscribe.
 */
export function discover(onChange: (wallets: Wallet[]) => void): () => void {
  if (typeof window === "undefined") {
    return () => {};
  }

  const found = new Map<string, Wallet>();

  const publish = () => onChange([...found.values()]);

  const announced = (event: Event) => {
    const detail = (event as CustomEvent<Announcement>).detail;
    if (!detail?.info?.rdns || !detail.provider) {
      return;
    }
    // Keyed by rdns rather than the per-page uuid, so a wallet that announces twice — which
    // happens when a page re-requests — appears once.
    found.set(detail.info.rdns, {
      id: detail.info.rdns,
      name: detail.info.name || detail.info.rdns,
      icon: detail.info.icon || "",
      provider: detail.provider,
    });
    publish();
  };

  window.addEventListener("eip6963:announceProvider", announced);
  window.dispatchEvent(new Event("eip6963:requestProvider"));

  // An older wallet that never announces is still usable; it just cannot say what it is, so
  // it is offered under a neutral name and only when nothing announced itself.
  if (found.size === 0 && window.ethereum) {
    found.set(legacyID, {
      id: legacyID,
      name: "Injected wallet",
      icon: "",
      provider: window.ethereum,
    });
  }
  publish();

  return () => window.removeEventListener("eip6963:announceProvider", announced);
}

/**
 * connect asks one wallet which account it will act as.
 *
 * This is the step that shows a prompt, so it is deliberately separate from signing: a
 * person approves being identified before they are asked to approve anything else.
 */
export async function connect(wallet: Wallet): Promise<string> {
  const accounts = await call<string[]>(wallet.provider, "eth_requestAccounts");
  const account = accounts?.[0];
  if (!account) {
    throw new WalletError("rejected", "No account was shared by the wallet.");
  }
  return account.toLowerCase();
}

/**
 * sign asks the wallet to sign a message with personal_sign.
 *
 * The message is passed as text rather than hex so the wallet shows a person what they are
 * approving. The server's challenge says in words that it authorizes no payment and moves no
 * funds, which is only useful if it is legible in the prompt.
 */
export async function sign(wallet: Wallet, message: string, account: string): Promise<string> {
  const signature = await call<string>(wallet.provider, "personal_sign", [message, account]);
  if (!signature) {
    throw new WalletError("failed", "The wallet returned no signature.");
  }
  return signature;
}

/** onAccountChange reports when a wallet switches accounts, and returns an unsubscribe. */
export function onAccountChange(
  wallet: Wallet | null,
  handler: (account: string | null) => void,
): () => void {
  const provider = wallet?.provider;
  if (!provider?.on || !provider.removeListener) {
    return () => {};
  }

  const listener = (...args: unknown[]) => {
    const shared = args[0] as string[] | undefined;
    handler(shared?.[0]?.toLowerCase() ?? null);
  };

  provider.on("accountsChanged", listener);
  return () => provider.removeListener?.("accountsChanged", listener);
}

/**
 * call translates a wallet's refusal into one of ours.
 *
 * 4001 is EIP-1193's "the user rejected the request", and it is not an error worth an alarm
 * on screen: someone decided not to sign, which is the prompt working.
 */
async function call<T>(provider: Eip1193, method: string, params?: unknown[]): Promise<T> {
  try {
    return (await provider.request({ method, params })) as T;
  } catch (cause) {
    const code = (cause as { code?: number }).code;
    if (code === 4001) {
      throw new WalletError("rejected", "The request was declined in the wallet.");
    }
    const message = cause instanceof Error ? cause.message : String(cause);
    throw new WalletError("failed", message);
  }
}
