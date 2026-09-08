import { describe, expect, it } from "vitest";

import { compact, money, percent, relative, shortHash, sum, tone, words } from "./format";

/**
 * The percentage tests are the ones that matter.
 *
 * A rate arrives as an exact decimal string and the obvious rendering — Number(rate) * 100 —
 * reintroduces exactly the floating-point error the whole backend is built to avoid, in the
 * one place a person reads the number. These cases pin the string arithmetic that replaces it.
 */
describe("percent", () => {
  it("shifts the decimal point rather than multiplying", () => {
    expect(percent("0.123602")).toBe("12.36%");
    expect(percent("0.0642")).toBe("6.42%");
    expect(percent("0.45")).toBe("45.00%");
    expect(percent("1")).toBe("100.00%");
    expect(percent("0")).toBe("0.00%");
  });

  it("keeps a value a float would round wrong", () => {
    // 0.29 * 100 is 28.999999999999996 in IEEE 754.
    expect(percent("0.29")).toBe("29.00%");
    // 1.005 rounds down under binary arithmetic; half away from zero rounds it up.
    expect(percent("0.010050", 2)).toBe("1.01%");
  });

  it("carries across a digit boundary", () => {
    expect(percent("0.099999", 2)).toBe("10.00%");
    expect(percent("0.0999999", 4)).toBe("10.0000%");
  });

  it("honours the requested precision", () => {
    expect(percent("0.123602", 0)).toBe("12%");
    expect(percent("0.123602", 4)).toBe("12.3602%");
  });

  it("keeps a sign only where there is something to sign", () => {
    expect(percent("-0.0125")).toBe("-1.25%");
    expect(percent("-0.000001")).toBe("0.00%");
  });
});

describe("words", () => {
  it("reads an action without its module", () => {
    expect(words("invoice.assessment_requested")).toBe("Assessment requested");
    expect(words("AUCTION_OPEN")).toBe("Auction open");
    expect(words("YIELD")).toBe("Yield");
  });
});

describe("tone", () => {
  it("maps a status to the meaning a colour may carry", () => {
    expect(tone("SETTLED")).toBe("is-done");
    expect(tone("OPEN")).toBe("is-live");
    expect(tone("TOKENIZING")).toBe("is-working");
    expect(tone("FAILED")).toBe("is-bad");
    expect(tone("SOMETHING_NEW")).toBe("");
  });
});

describe("shortHash", () => {
  it("shortens only what is long enough to need it", () => {
    expect(shortHash("0x1234")).toBe("0x1234");
    expect(shortHash("0x" + "a".repeat(64))).toBe("0xaaaaaa…aaaa");
  });
});

describe("relative", () => {
  const now = new Date("2026-09-08T12:00:00Z");

  it("says how long is left in a window", () => {
    expect(relative("2026-09-09T12:00:00Z", now)).toBe("in 24 hours");
    expect(relative("2026-09-08T12:30:00Z", now)).toBe("in 30 minutes");
    expect(relative("2026-09-08T11:59:30Z", now)).toBe("30 seconds ago");
    expect(relative("2026-09-01T12:00:00Z", now)).toBe("7 days ago");
  });

  it("does not say 1 hours", () => {
    expect(relative("2026-09-08T13:00:00Z", now)).toBe("in 1 hour");
  });
});

describe("money", () => {
  it("groups without ever making the value a number", () => {
    expect(money("20060327.41", "USD")).toBe("$20,060,327.41");
    expect(money("999.00", "USD")).toBe("$999.00");
    expect(money("1000", "USD")).toBe("$1,000");
    expect(money("-1234567.89", "USD")).toBe("-$1,234,567.89");
  });

  it("keeps a code a reader would otherwise have to guess", () => {
    expect(money("1234.50", "EUR")).toBe("1,234.50 EUR");
  });

  it("survives a value wider than a double can hold exactly", () => {
    expect(money("9007199254740993.01", "USD")).toBe("$9,007,199,254,740,993.01");
  });
});

/**
 * A total is where a screen is most tempted to do arithmetic, and where a float error would
 * make the header disagree with the rows underneath it.
 */
describe("sum", () => {
  it("adds exactly", () => {
    expect(sum(["10000.00", "5000.50", "0.49"])).toBe("15000.99");
    expect(sum([])).toBe("0.00");
    expect(sum(["21000.00"])).toBe("21000.00");
  });

  it("adds values a float would get wrong", () => {
    expect(sum(["0.1", "0.2"])).toBe("0.3");
    expect(sum(["9007199254740992.00", "1.00"])).toBe("9007199254740993.00");
  });

  it("lines up amounts written to different scales", () => {
    expect(sum(["1.5", "2.25", "3"])).toBe("6.75");
  });
});

describe("compact", () => {
  it("shortens a total to what a header has room for", () => {
    expect(compact("1443578760.47")).toBe("$1.44B");
    expect(compact("346696296.49")).toBe("$346.69M");
    expect(compact("21000.00")).toBe("$21.00K");
    expect(compact("980.00")).toBe("$980");
  });
});
