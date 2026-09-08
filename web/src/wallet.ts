/**
 * The browser wallet, through EIP-1193.
 *
 * Nothing here holds a key or produces a signature: the wallet does both, behind a prompt
 * the person answers. What this module owns is the small, awkward surface around that —
 * whether a wallet is present at all, which account it will act as, and what its refusals
 * mean, because "user rejected" and "no wallet installed" need different words on screen.
 */

/** EIP-1193 is the interface every injected wallet agrees to expose. */
interface Eip1193 {
  request(args: { method: string; params?: unknown[] }): Promise<unknown>;
  on?(event: string, handler: (...args: unknown[]) => void): void;
  removeListener?(event: string, handler: (...args: unknown[]) => void): void;
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

/** provider returns the injected wallet, or null when the browser has none. */
export function provider(): Eip1193 | null {
  return typeof window === "undefined" ? null : (window.ethereum ?? null);
}

export function walletAvailable(): boolean {
  return provider() !== null;
}

/**
 * connect asks the wallet which account it will act as.
 *
 * This is the step that shows a prompt, so it is deliberately separate from signing: a
 * person approves being identified before they are asked to approve anything else.
 */
export async function connect(): Promise<string> {
  const wallet = required();

  const accounts = await call<string[]>(wallet, "eth_requestAccounts");
  const account = accounts?.[0];
  if (!account) {
    throw new WalletError("rejected", "No account was shared by the wallet.");
  }
  return account.toLowerCase();
}

/** accounts returns the already-authorized accounts without prompting. */
export async function accounts(): Promise<string[]> {
  const wallet = provider();
  if (!wallet) {
    return [];
  }
  const shared = await call<string[]>(wallet, "eth_accounts").catch(() => []);
  return (shared ?? []).map((account) => account.toLowerCase());
}

/**
 * sign asks the wallet to sign a message with personal_sign.
 *
 * The message is passed as text rather than hex so the wallet shows a person what they are
 * approving. The server's challenge says in words that it authorizes no payment and moves
 * no funds, which is only useful if it is legible in the prompt.
 */
export async function sign(message: string, account: string): Promise<string> {
  const wallet = required();

  const signature = await call<string>(wallet, "personal_sign", [message, account]);
  if (!signature) {
    throw new WalletError("failed", "The wallet returned no signature.");
  }
  return signature;
}

/** onAccountChange reports when the wallet switches accounts, and returns an unsubscribe. */
export function onAccountChange(handler: (account: string | null) => void): () => void {
  const wallet = provider();
  if (!wallet?.on || !wallet.removeListener) {
    return () => {};
  }

  const listener = (...args: unknown[]) => {
    const shared = args[0] as string[] | undefined;
    handler(shared?.[0]?.toLowerCase() ?? null);
  };

  wallet.on("accountsChanged", listener);
  return () => wallet.removeListener?.("accountsChanged", listener);
}

function required(): Eip1193 {
  const wallet = provider();
  if (!wallet) {
    throw new WalletError("absent", "No wallet was found in this browser.");
  }
  return wallet;
}

/**
 * call translates a wallet's refusal into one of ours.
 *
 * 4001 is EIP-1193's "the user rejected the request", and it is not an error worth an alarm
 * on screen: someone decided not to sign, which is the prompt working.
 */
async function call<T>(wallet: Eip1193, method: string, params?: unknown[]): Promise<T> {
  try {
    return (await wallet.request({ method, params })) as T;
  } catch (cause) {
    const code = (cause as { code?: number }).code;
    if (code === 4001) {
      throw new WalletError("rejected", "The request was declined in the wallet.");
    }
    const message = cause instanceof Error ? cause.message : String(cause);
    throw new WalletError("failed", message);
  }
}
