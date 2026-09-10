import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import { api, ApiError, sessionLost } from "./api/client";
import * as wallet from "./wallet";
import type { Wallet } from "./wallet";

/**
 * Who the caller is.
 *
 * A wallet proves itself to the server and gets a session cookie; the browser never holds a
 * key and cannot read the cookie. Everything the interface then allows follows from what the
 * server says about that organization, not from anything decided here.
 */
export interface Actor {
  id: string;
  wallet: string;
  name: string;
  type: "ISSUER" | "INVESTOR" | "OPERATOR" | "";
  eligible: boolean;
  operator: boolean;
}

const anonymous: Actor = {
  id: "",
  wallet: "",
  name: "",
  type: "",
  eligible: false,
  operator: false,
};

export type Stage =
  /** The cookie has not been checked yet. */
  | "loading"
  /** Nobody is signed in. */
  | "anonymous"
  /** A wallet proved itself, but no organization has been registered for it. */
  | "unregistered"
  | "signed-in";

/**
 * The seeded participants, for a browser with no wallet.
 *
 * Their identifiers are the ones the seed writes, and the seed derives them from a fixed
 * namespace so a screen can name them. Choosing one here sends a header the API honours only
 * in development, and even then the server reads what that organization may do from its own
 * record — so this can never grant anything a wallet session would not have.
 */
export interface Participant {
  id: string;
  name: string;
  role: "ISSUER" | "INVESTOR" | "OPERATOR";
}

export const participants: Participant[] = [
  { id: "00000000-0000-4000-8000-000000000002", name: "Northwind Trading GmbH", role: "ISSUER" },
  { id: "00000000-0000-4000-8000-000000000003", name: "Baltic Freight OÜ", role: "ISSUER" },
  { id: "00000000-0000-4000-8000-000000000004", name: "Alpine Treasury AG", role: "INVESTOR" },
  { id: "00000000-0000-4000-8000-000000000005", name: "Meridian Credit Fund", role: "INVESTOR" },
  { id: "00000000-0000-4000-8000-000000000001", name: "FactorFlow Operations", role: "OPERATOR" },
];

const demoKey = "factorflow.demo-participant";
const walletKey = "factorflow.wallet";

interface Session {
  stage: Stage;
  /** demoAuth reports whether this deployment honours the seeded-participant shortcut. */
  demoAuth: boolean;
  actor: Actor;
  /** auth is the development header value, and empty for a real wallet session. */
  auth: string;
  /** pendingWallet is the address that proved itself but has no organization yet. */
  pendingWallet: string;
  /** wallets are the browser extensions that announced themselves, in announcement order. */
  wallets: Wallet[];
  walletAvailable: boolean;
  busy: boolean;
  error: unknown;

  isIssuer: boolean;
  isInvestor: boolean;
  isOperator: boolean;
  nameOf: (organizationID: string) => string;

  connect: (walletID?: string) => Promise<void>;
  register: (type: string, name: string) => Promise<void>;
  signOut: () => Promise<void>;
  useDemo: (participantID: string) => void;
  dismissError: () => void;
}

const SessionContext = createContext<Session | null>(null);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [stage, setStage] = useState<Stage>("loading");
  const [actor, setActor] = useState<Actor>(anonymous);
  const [auth, setAuth] = useState("");
  const [pendingWallet, setPendingWallet] = useState("");
  const [busy, setBusy] = useState(false);
  const [demoAuth, setDemoAuth] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [wallets, setWallets] = useState<Wallet[]>([]);
  const [chosen, setChosen] = useState<Wallet | null>(null);

  // Wallets announce themselves rather than fighting over one global, so the list is a
  // subscription: an extension that loads late appears without the page being reloaded.
  useEffect(() => wallet.discover(setWallets), []);

  /**
   * load asks the server who the caller is.
   *
   * The answer comes from the session cookie, so a reload keeps a person signed in without
   * the app storing anything itself. The organization is then read for its name and type,
   * which the session endpoint deliberately does not carry.
   */
  const load = useCallback(async (header: string): Promise<boolean> => {
    try {
      const identity = await api.me(header);
      const organization = await api
        .organization(header, identity.organization_id)
        .catch(() => null);

      setActor({
        id: identity.organization_id,
        wallet: identity.wallet,
        name: organization?.name ?? identity.wallet,
        type: (organization?.type as Actor["type"]) ?? "",
        eligible: identity.eligible,
        operator: identity.operator,
      });
      setAuth(header);
      setStage("signed-in");
      return true;
    } catch {
      setActor(anonymous);
      setAuth("");
      setStage("anonymous");
      return false;
    }
  }, []);

  // A signed-in session survives a reload; a chosen demo participant survives one too, so a
  // developer is not asked to pick again on every edit.
  useEffect(() => {
    let live = true;

    (async () => {
      const restored = await load("");
      if (!live || restored) {
        return;
      }
      const saved = localStorage.getItem(demoKey);
      if (saved && participants.some((p) => p.id === saved)) {
        await load(saved);
      }
    })();

    return () => {
      live = false;
    };
  }, [load]);

  /**
   * What this deployment allows is asked once, before anything is offered.
   *
   * It defaults to false: a screen that offered a shortcut the server refuses would show a
   * visitor an error where it meant to show a way in, and defaulting the other way makes
   * that the behaviour whenever the question fails.
   */
  useEffect(() => {
    let live = true;
    void (async () => {
      try {
        const config = await api.config();
        if (live) {
          setDemoAuth(config.demo_auth);
        }
      } catch {
        // Any failure to ask leaves the shortcut hidden, which is the safe direction: a
        // shortcut the server refuses sends a visitor into an error, and a missing one only
        // asks them to use a wallet.
      }
    })();
    return () => {
      live = false;
    };
  }, []);

  /**
   * A session can end while the app is open: the cookie's twelve hours run out, or somebody
   * signs out in another tab. The next request says 401, and the app returns to the sign-in
   * screen rather than rendering a refusal on every panel.
   */
  useEffect(() => {
    const lost = () => {
      setActor(anonymous);
      setAuth("");
      setPendingWallet("");
      setStage((current) => (current === "signed-in" ? "anonymous" : current));
    };
    window.addEventListener(sessionLost, lost);
    return () => window.removeEventListener(sessionLost, lost);
  }, []);

  /** The wallet switching accounts means a different person is at the keyboard. */
  useEffect(
    () =>
      wallet.onAccountChange(chosen, (account) => {
        if (actor.wallet && account !== actor.wallet) {
          void api.logout().catch(() => {});
          setActor(anonymous);
          setAuth("");
          setPendingWallet("");
          setStage("anonymous");
        }
      }),
    [actor.wallet, chosen],
  );

  /** pick resolves which wallet a request is for: the named one, or the only one there is. */
  const pick = useCallback(
    (walletID?: string): Wallet => {
      const remembered = walletID ?? localStorage.getItem(walletKey) ?? "";
      const found =
        wallets.find((candidate) => candidate.id === remembered) ??
        (wallets.length === 1 ? wallets[0] : undefined);

      if (!found) {
        throw new wallet.WalletError(
          "absent",
          wallets.length === 0
            ? "No wallet was found in this browser."
            : "Choose which wallet to sign with.",
        );
      }
      return found;
    },
    [wallets],
  );

  const connect = useCallback(
    async (walletID?: string) => {
      setBusy(true);
      setError(null);
      try {
        const using = pick(walletID);
        setChosen(using);
        localStorage.setItem(walletKey, using.id);

        const account = await wallet.connect(using);
        const challenge = await api.challenge(account);
        const signature = await wallet.sign(using, challenge.message, account);

        try {
          await api.verify(challenge.nonce, signature);
        } catch (cause) {
          // A well-formed address that no organization claims is not a failure: it is
          // someone who has not registered yet, and the next screen is the form rather
          // than an error.
          if (cause instanceof ApiError && cause.status === 403) {
            setPendingWallet(account);
            setStage("unregistered");
            return;
          }
          throw cause;
        }

        await load("");
      } catch (cause) {
        setError(cause);
      } finally {
        setBusy(false);
      }
    },
    [load, pick],
  );

  /**
   * register creates the organization behind a proven wallet.
   *
   * It signs a second, fresh challenge rather than reusing the one that just failed: a nonce
   * is spent by the request that verifies it, and asking the wallet again is honest about
   * what is being authorized — this signature creates an organization.
   */
  const register = useCallback(
    async (type: string, name: string) => {
      setBusy(true);
      setError(null);
      try {
        const using = chosen ?? pick();
        const account = pendingWallet || (await wallet.connect(using));
        const challenge = await api.challenge(account);
        const signature = await wallet.sign(using, challenge.message, account);

        await api.register(challenge.nonce, signature, type, name);

        const next = await api.challenge(account);
        await api.verify(next.nonce, await wallet.sign(using, next.message, account));
        await load("");
        setPendingWallet("");
      } catch (cause) {
        setError(cause);
      } finally {
        setBusy(false);
      }
    },
    [load, pendingWallet, chosen, pick],
  );

  const signOut = useCallback(async () => {
    setBusy(true);
    try {
      await api.logout().catch(() => {});
      localStorage.removeItem(demoKey);
      localStorage.removeItem(walletKey);
      setChosen(null);
      setActor(anonymous);
      setAuth("");
      setPendingWallet("");
      setStage("anonymous");
    } finally {
      setBusy(false);
    }
  }, []);

  const useDemo = useCallback(
    (participantID: string) => {
      const participant = participants.find((p) => p.id === participantID);
      if (!participant) {
        return;
      }
      localStorage.setItem(demoKey, participant.id);

      // The header names an organization; everything about it still comes from the server.
      setBusy(true);
      void load(participant.id).finally(() => setBusy(false));
    },
    [load],
  );

  const value = useMemo<Session>(
    () => ({
      stage,
      demoAuth,
      actor,
      auth,
      pendingWallet,
      wallets,
      walletAvailable: wallets.length > 0,
      busy,
      error,
      isIssuer: actor.type === "ISSUER",
      isInvestor: actor.type === "INVESTOR",
      isOperator: actor.operator,
      // An identifier on its own tells a reader nothing, so a counterparty the demo knows is
      // named, and one it does not know keeps its id rather than being hidden.
      nameOf: (organizationID: string) =>
        organizationID === actor.id
          ? actor.name || organizationID
          : (participants.find((p) => p.id === organizationID)?.name ?? organizationID),
      connect,
      register,
      signOut,
      useDemo,
      dismissError: () => setError(null),
    }),
    [
      stage,
      demoAuth,
      actor,
      auth,
      pendingWallet,
      wallets,
      busy,
      error,
      connect,
      register,
      signOut,
      useDemo,
    ],
  );

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): Session {
  const session = useContext(SessionContext);
  if (!session) {
    throw new Error("useSession must be used inside a SessionProvider");
  }
  return session;
}
