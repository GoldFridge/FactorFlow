import { createContext, useContext, useMemo, useState, type ReactNode } from "react";

/**
 * The demo participants.
 *
 * Their identifiers are the ones the seed writes, and the seed derives them from a fixed
 * namespace precisely so a screen can name them. Nothing here grants any authority: the
 * server reads what an organization may do from its own record, so picking a name in this
 * switcher can only ever narrow what the API will allow, never widen it.
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

const storageKey = "factorflow.participant";

interface Session {
  actor: Participant;
  setActor: (id: string) => void;
  isIssuer: boolean;
  isInvestor: boolean;
  isOperator: boolean;
  nameOf: (organizationID: string) => string;
}

const SessionContext = createContext<Session | null>(null);

function stored(): Participant {
  const saved = localStorage.getItem(storageKey);
  return participants.find((p) => p.id === saved) ?? participants[0]!;
}

export function SessionProvider({ children }: { children: ReactNode }) {
  const [actor, setCurrent] = useState<Participant>(stored);

  const value = useMemo<Session>(
    () => ({
      actor,
      setActor: (id: string) => {
        const next = participants.find((p) => p.id === id);
        if (next) {
          localStorage.setItem(storageKey, next.id);
          setCurrent(next);
        }
      },
      isIssuer: actor.role === "ISSUER",
      isInvestor: actor.role === "INVESTOR",
      isOperator: actor.role === "OPERATOR",
      // An id on its own tells a reader nothing. Where the API returns one for a
      // counterparty the demo knows, the name is shown instead, and the id is kept for
      // anyone the demo does not know rather than being hidden.
      nameOf: (organizationID: string) =>
        participants.find((p) => p.id === organizationID)?.name ?? organizationID,
    }),
    [actor],
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
