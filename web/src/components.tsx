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

/** Stat states one number about the whole venue, with a watermark that cannot be misread. */
export function Stat({
  label,
  value,
  mark,
}: {
  label: string;
  value: string;
  mark?: string;
}) {
  return (
    <dl className="stat">
      <dt>{label}</dt>
      <dd>{value}</dd>
      {mark ? (
        <span className="watermark" aria-hidden="true">
          {mark}
        </span>
      ) : null}
    </dl>
  );
}

export interface Choice {
  id: string;
  label: string;
  count?: number;
}

/** Chips are the filter row: one choice at a time, each carrying how much it holds. */
export function Chips({
  choices,
  current,
  onChoose,
}: {
  choices: Choice[];
  current: string;
  onChoose: (id: string) => void;
}) {
  return (
    <div className="chips" role="tablist">
      {choices.map((choice) => (
        <button
          key={choice.id}
          role="tab"
          aria-selected={choice.id === current}
          className={`chip-filter ${choice.id === current ? "is-on" : ""}`}
          onClick={() => onChoose(choice.id)}
        >
          {choice.label}
          {choice.count === undefined ? null : <span className="count">{choice.count}</span>}
        </button>
      ))}
    </div>
  );
}

export function Search({
  value,
  onChange,
  placeholder,
}: {
  value: string;
  onChange: (value: string) => void;
  placeholder: string;
}) {
  return (
    <div className="search">
      <span className="glass" aria-hidden="true">
        <Glass />
      </span>
      <input
        type="search"
        aria-label={placeholder}
        placeholder={placeholder}
        value={value}
        onChange={(event) => onChange(event.target.value)}
      />
    </div>
  );
}

/**
 * Discs are the row's subject rendered as overlapping counters — here the debtor's initial
 * and the grade its receivable carries, which are the two things a scanning eye sorts on.
 */
export function Discs({ initial, grade }: { initial: string; grade?: string }) {
  return (
    <span className="discs" aria-hidden="true">
      <span className="disc">{initial}</span>
      {grade ? <span className={`disc is-grade-${grade}`}>{grade}</span> : null}
    </span>
  );
}

/**
 * Metered puts a number over a bar showing its share of the largest value on screen. The
 * comparison is within the list and nowhere else, which is the only one the number invites.
 */
export function Metered({ value, share }: { value: string; share: number }) {
  const width = Math.max(0, Math.min(100, share * 100));
  return (
    <span className="metered">
      <span className="value">{value}</span>
      <span className="meter">
        <span className="fill" style={{ width: `${width}%` }} />
      </span>
    </span>
  );
}

export function IconButton({
  label,
  children,
  onClick,
}: {
  label: string;
  children: ReactNode;
  onClick: () => void;
}) {
  return (
    <button className="icon-button" title={label} aria-label={label} onClick={onClick}>
      {children}
    </button>
  );
}

/*
 * The icons are drawn here rather than pulled from a set.
 *
 * There are four of them, each a handful of strokes, and they inherit the current colour so
 * they follow the button they sit in. A dependency for that would be more bytes than the
 * app's own code.
 */

function stroke(paths: ReactNode) {
  return (
    <svg
      width="16"
      height="16"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      {paths}
    </svg>
  );
}

/** Glass is the search affordance. */
export function Glass() {
  return stroke(
    <>
      <circle cx="7" cy="7" r="4.25" />
      <path d="M10.2 10.2 13.5 13.5" />
    </>,
  );
}

/** Pulse reads as "the reasoning behind this number". */
export function Pulse() {
  return stroke(<path d="M1.5 8.5h2.6L6 4.2l2.4 8L10.4 8.5h4.1" />);
}

/** Expand reads as "open this record". */
export function Expand() {
  return stroke(
    <>
      <path d="M9.5 2.5h4v4" />
      <path d="M13.5 2.5 8.6 7.4" />
      <path d="M13.5 9.8v2.2a1.5 1.5 0 0 1-1.5 1.5H4a1.5 1.5 0 0 1-1.5-1.5V4a1.5 1.5 0 0 1 1.5-1.5h2.2" />
    </>,
  );
}
