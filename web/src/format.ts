/**
 * Display formatting.
 *
 * Everything here takes a string from the API and returns a string for a screen. Nothing
 * computes: a decimal that arrived exact must not be turned into a float on its way to a
 * reader, so a rate is rendered by moving its decimal point rather than by multiplying.
 */

/** money renders an amount with its currency, keeping the digits the server sent. */
export function money(amount: string, currency: string): string {
  return `${amount} ${currency}`;
}

/**
 * percent renders a decimal rate as a percentage.
 *
 * The shift is done on the string — "0.123602" becomes "12.36%" — because multiplying by
 * 100 in floating point is exactly the rounding this codebase avoids everywhere else.
 */
export function percent(rate: string, decimals = 2): string {
  const negative = rate.startsWith("-");
  const [whole = "0", fraction = ""] = rate.replace("-", "").split(".");

  const digits = (whole + fraction).padEnd(whole.length + Math.max(fraction.length, 2), "0");
  const point = whole.length + 2;
  const shiftedWhole = digits.slice(0, point).replace(/^0+(?=\d)/, "");
  const shiftedFraction = digits.slice(point);

  const rounded = round(shiftedWhole || "0", shiftedFraction, decimals);
  // A value that rounds away to nothing is not a negative quantity, and "-0.00%" reads as
  // if it were one.
  const vanished = /^0(\.0*)?$/.test(rounded);
  return `${negative && !vanished ? "-" : ""}${rounded}%`;
}

/** round trims a decimal string to a number of places, rounding half away from zero. */
function round(whole: string, fraction: string, decimals: number): string {
  if (fraction.length <= decimals) {
    return decimals > 0 ? `${whole}.${fraction.padEnd(decimals, "0")}` : whole;
  }

  const kept = fraction.slice(0, decimals);
  const next = Number(fraction[decimals] ?? "0");
  if (next < 5) {
    return decimals > 0 ? `${whole}.${kept}` : whole;
  }

  // Carry by hand: the value may be wider than a float can hold exactly.
  const carried = (BigInt(whole + kept) + 1n).toString().padStart(kept.length + 1, "0");
  const boundary = carried.length - decimals;
  const wholePart = carried.slice(0, boundary) || "0";
  return decimals > 0 ? `${wholePart}.${carried.slice(boundary)}` : wholePart;
}

/** shortHash renders a digest as its first and last characters. */
export function shortHash(hash: string, keep = 8): string {
  if (hash.length <= keep * 2 + 3) {
    return hash;
  }
  return `${hash.slice(0, keep)}…${hash.slice(-4)}`;
}

const dateFormat = new Intl.DateTimeFormat("en-GB", {
  day: "2-digit",
  month: "short",
  year: "numeric",
});

const timeFormat = new Intl.DateTimeFormat("en-GB", {
  day: "2-digit",
  month: "short",
  hour: "2-digit",
  minute: "2-digit",
});

export function date(iso: string): string {
  return dateFormat.format(new Date(iso));
}

export function dateTime(iso: string): string {
  return timeFormat.format(new Date(iso));
}

/** untilNow says how long is left, for a window a person is deciding inside of. */
export function relative(iso: string, from: Date = new Date()): string {
  const seconds = Math.round((new Date(iso).getTime() - from.getTime()) / 1000);
  const past = seconds < 0;
  const magnitude = Math.abs(seconds);

  // The boundaries are the units themselves rather than round numbers near them: an hour
  // away should read as an hour, not as sixty minutes.
  const [value, unit] =
    magnitude < 90
      ? [magnitude, "second"]
      : magnitude < 3600
        ? [Math.round(magnitude / 60), "minute"]
        : magnitude < 172800
          ? [Math.round(magnitude / 3600), "hour"]
          : [Math.round(magnitude / 86400), "day"];

  const plural = value === 1 ? unit : `${unit}s`;
  return past ? `${value} ${plural} ago` : `in ${value} ${plural}`;
}

/** words turns an action or status into something readable: invoice.assessed → Assessed. */
export function words(token: string): string {
  const tail = token.includes(".") ? token.slice(token.indexOf(".") + 1) : token;
  const spaced = tail.replace(/[._]/g, " ").toLowerCase();
  return spaced.charAt(0).toUpperCase() + spaced.slice(1);
}

/**
 * tone maps a status to the meaning a colour is allowed to carry: something is finished,
 * something is in flight, or something went wrong.
 */
export function tone(status: string): "" | "is-live" | "is-working" | "is-done" | "is-bad" {
  switch (status) {
    case "SETTLED":
    case "TOKENIZED":
    case "ALLOCATED":
    case "CLEARED":
    case "ACCOUNTED":
    case "APPROVED":
    case "ELIGIBLE":
      return "is-done";
    case "OPEN":
    case "AUCTION_OPEN":
    case "ACTIVE":
      return "is-live";
    case "EXTRACTING":
    case "TOKENIZING":
    case "CLEARING":
    case "SETTLING":
    case "ASSESSED":
    case "PENDING":
    case "SUBMITTED":
      return "is-working";
    case "FAILED":
    case "REJECTED":
    case "CANCELLED":
    case "EXPIRED":
      return "is-bad";
    default:
      return "";
  }
}
