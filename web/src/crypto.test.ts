import { beforeEach, describe, expect, it } from "vitest";

import { digest, fromBase64, open, recallKey, rememberKey, seal, toBase64 } from "./crypto";

const document = new TextEncoder().encode(
  "INVOICE INV-2026-0007\nACME Logistics GmbH\n15000.00 USD\nDue 2026-11-07",
);

describe("sealing a document", () => {
  /**
   * The round trip is the property the upload rests on: what the browser sealed is what the
   * browser can open, and nothing in between needed the key.
   */
  it("opens only with the key it produced", async () => {
    const sealed = await seal(document.buffer as ArrayBuffer);

    const opened = await open(sealed.ciphertext, sealed.key);
    expect(new TextDecoder().decode(opened)).toContain("INV-2026-0007");

    const other = await seal(document.buffer as ArrayBuffer);
    await expect(open(sealed.ciphertext, other.key)).rejects.toThrow();
  });

  /** A fresh nonce per document means the same invoice never encrypts to the same bytes. */
  it("never produces the same ciphertext twice", async () => {
    const first = await seal(document.buffer as ArrayBuffer);
    const second = await seal(document.buffer as ArrayBuffer);

    expect(toBase64(first.ciphertext)).not.toBe(toBase64(second.ciphertext));
    expect(first.key).not.toBe(second.key);
  });

  /*
   * AES-GCM authenticates as well as encrypts, so a ciphertext that was altered fails to
   * open rather than decrypting into something plausible. That is what lets the stored
   * digest stand for the document.
   */
  it("refuses a ciphertext that was tampered with", async () => {
    const sealed = await seal(document.buffer as ArrayBuffer);
    const altered = new Uint8Array(sealed.ciphertext);
    altered[altered.length - 1] = (altered.at(-1) ?? 0) ^ 0x01;

    await expect(open(altered, sealed.key)).rejects.toThrow();
  });
});

describe("the digest", () => {
  /** The server computes the same digest over the bytes it stored; these must agree. */
  it("is the SHA-256 of the ciphertext, in lowercase hex", async () => {
    const known = new TextEncoder().encode("abc");
    await expect(digest(known)).resolves.toBe(
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
    );

    const sealed = await seal(document.buffer as ArrayBuffer);
    await expect(digest(sealed.ciphertext)).resolves.toMatch(/^[0-9a-f]{64}$/);
  });
});

describe("base64", () => {
  it("survives bytes a text encoding would mangle", () => {
    const bytes = new Uint8Array(256);
    for (let i = 0; i < bytes.length; i++) {
      bytes[i] = i;
    }
    expect([...fromBase64(toBase64(bytes))]).toEqual([...bytes]);
  });

  /** Large documents are encoded in chunks, because one spread argument overflows the stack. */
  it("encodes a document larger than the call stack allows in one go", () => {
    const large = new Uint8Array(300_000).fill(7);
    expect(fromBase64(toBase64(large)).length).toBe(large.length);
  });
});

describe("keys", () => {
  beforeEach(() => localStorage.clear());

  /**
   * A key the platform could recover is a key the platform could use, so it stays here. That
   * is the honest consequence of the design, and the reason this is worth a test at all.
   */
  it("are kept in the browser and nowhere else", async () => {
    const sealed = await seal(document.buffer as ArrayBuffer);
    rememberKey("invoice-1", sealed.key);

    expect(recallKey("invoice-1")).toBe(sealed.key);
    expect(recallKey("invoice-2")).toBeNull();

    const kept = JSON.stringify(localStorage);
    expect(kept).toContain(sealed.key);
    expect(kept).not.toContain("INV-2026-0007");
  });

  it("survive a storage that refuses to answer", () => {
    localStorage.setItem("factorflow.document-keys", "not json");
    expect(recallKey("invoice-1")).toBeNull();
  });
});
