import { useCallback, useEffect, useState, type ReactNode } from "react";

import { ApiError } from "./api/client";
import { tone, words } from "./format";

/** Panel is the one container: a titled block with a border. */
export function Panel({
  title,
  aside,
  children,
  padded = true,
}: {
  title: string;
  aside?: ReactNode;
  children: ReactNode;
  padded?: boolean;
}) {
  return (
    <section className="panel">
      <header>
        <h2>{title}</h2>
        {aside}
      </header>
      {padded ? <div className="panel-body">{children}</div> : children}
    </section>
  );
}

export function Status({ value }: { value: string }) {
  return <span className={`pill ${tone(value)}`}>{words(value)}</span>;
}

export function Grade({ value }: { value: string }) {
  return (
    <span className="grade" data-grade={value} title={`Risk grade ${value}`}>
      {value}
    </span>
  );
}

export function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="fact">
      <dt>{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}

export function Headline({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="fact">
      <dt>{label}</dt>
      <dd className="big">{children}</dd>
    </div>
  );
}

/**
 * Failure explains a refusal in the words the server used.
 *
 * The trace id is shown rather than swallowed: it is what connects what a person is looking
 * at to the request the server logged, and a demo where nobody can do that is a demo where
 * every failure is a mystery.
 */
export function Failure({ error }: { error: unknown }) {
  if (!error) {
    return null;
  }
  const message = error instanceof Error ? error.message : String(error);
  const trace = error instanceof ApiError ? error.traceId : "";

  return (
    <div className="notice is-error" role="alert">
      {message}
      {trace ? <span className="trace">trace {trace}</span> : null}
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <p className="empty">{children}</p>;
}

interface Async<T> {
  data: T | null;
  error: unknown;
  loading: boolean;
  reload: () => void;
}

/**
 * useAsync loads data and keeps the last good value while reloading.
 *
 * Blanking the screen on every refresh would make an operator lose their place, so a reload
 * replaces the numbers only once new ones have arrived.
 */
export function useAsync<T>(load: () => Promise<T>, deps: unknown[]): Async<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [loading, setLoading] = useState(true);
  const [nonce, setNonce] = useState(0);

  // The loader closes over the caller's dependencies, which are the real inputs here.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const run = useCallback(load, deps);

  useEffect(() => {
    let live = true;
    setLoading(true);

    run().then(
      (value) => {
        if (live) {
          setData(value);
          setError(null);
          setLoading(false);
        }
      },
      (cause: unknown) => {
        if (live) {
          setError(cause);
          setLoading(false);
        }
      },
    );

    return () => {
      live = false;
    };
  }, [run, nonce]);

  return { data, error, loading, reload: () => setNonce((n) => n + 1) };
}

/** Loader renders the three states a fetched panel can be in. */
export function Loader<T>({
  state,
  children,
  empty = "Nothing here yet.",
}: {
  state: Async<T>;
  children: (data: T) => ReactNode;
  empty?: string;
}) {
  if (state.error) {
    return <Failure error={state.error} />;
  }
  if (state.data === null) {
    return <Empty>{state.loading ? "Loading…" : empty}</Empty>;
  }
  return <>{children(state.data)}</>;
}
