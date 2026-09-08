/**
 * Document encryption, in the browser.
 *
 * The point of doing it here is what does not happen afterwards: the key is generated in this
 * tab and never sent, so the ciphertext the platform stores is something no operator, no
 * backup and no leak can turn back into an invoice. The server checks the digest of what it
 * received, which binds the assessment to those exact bytes without ever seeing inside them.
 *
 * AES-GCM with a 256-bit key and a fresh 12-byte nonce per document, which is the modern
 * default: it authenticates as well as encrypts, so a ciphertext that was tampered with fails
 * to open rather than decrypting into something plausible.
 */

const algorithm = "AES-GCM";
const nonceBytes = 12;

/** Sealed is an encrypted document and the key that opens it. */
export interface Sealed {
  /** ciphertext is the nonce followed by the sealed bytes, so one blob is self-contained. */
  ciphertext: Uint8Array;
  /** key is base64, and belongs to the person who uploaded the document. */
  key: string;
}

/**
 * bytesOf reads a file.
 *
 * Blob.arrayBuffer is the direct way and is used where it exists; FileReader is the fallback,
 * because it is the one path every browser has had for a decade — and, usefully, the one a
 * test environment provides too.
 */
export function bytesOf(file: Blob): Promise<ArrayBuffer> {
  if (typeof file.arrayBuffer === "function") {
    return file.arrayBuffer();
  }
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result as ArrayBuffer);
    reader.onerror = () => reject(reader.error ?? new Error("The file could not be read."));
    reader.readAsArrayBuffer(file);
  });
}

/**
 * seal encrypts a file under a key generated for it.
 *
 * The bytes are copied into a buffer this context owns before they reach WebCrypto. A buffer
 * that came from somewhere else — a file reader in another realm, most often — is rejected by
 * an identity check rather than by anything about its contents, and the error that produces
 * says nothing useful about the cause.
 */
export async function seal(file: ArrayBuffer | ArrayBufferView): Promise<Sealed> {
  const key = await crypto.subtle.generateKey({ name: algorithm, length: 256 }, true, [
    "encrypt",
    "decrypt",
  ]);

  const plaintext = copy(
    ArrayBuffer.isView(file)
      ? new Uint8Array(file.buffer, file.byteOffset, file.byteLength)
      : new Uint8Array(file),
  );

  const nonce = crypto.getRandomValues(new Uint8Array(new ArrayBuffer(nonceBytes)));
  const sealed = await crypto.subtle.encrypt({ name: algorithm, iv: nonce }, key, plaintext);

  const ciphertext = new Uint8Array(new ArrayBuffer(nonce.length + sealed.byteLength));
  ciphertext.set(nonce, 0);
  ciphertext.set(new Uint8Array(sealed), nonce.length);

  const raw = await crypto.subtle.exportKey("raw", key);
  return { ciphertext, key: toBase64(new Uint8Array(raw)) };
}

/** open decrypts what seal produced, given the key that was kept. */
export async function open(ciphertext: Uint8Array, key: string): Promise<Uint8Array> {
  const material = await crypto.subtle.importKey(
    "raw",
    fromBase64(key),
    { name: algorithm },
    false,
    ["decrypt"],
  );

  const nonce = copy(ciphertext.slice(0, nonceBytes));
  const sealed = copy(ciphertext.slice(nonceBytes));
  const plain = await crypto.subtle.decrypt({ name: algorithm, iv: nonce }, material, sealed);
  return new Uint8Array(plain);
}

/** digest is the SHA-256 of the ciphertext, the same one the server computes over it. */
export async function digest(bytes: Uint8Array): Promise<string> {
  const sum = await crypto.subtle.digest("SHA-256", copy(bytes));
  return [...new Uint8Array(sum)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

/** copy puts bytes on a plain ArrayBuffer, which is what WebCrypto's types ask for. */
function copy(bytes: Uint8Array): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(new ArrayBuffer(bytes.byteLength));
  out.set(bytes);
  return out;
}

export function toBase64(bytes: Uint8Array): string {
  // Chunked, because a large document as one spread argument overflows the call stack.
  let binary = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(binary);
}

export function fromBase64(value: string): Uint8Array<ArrayBuffer> {
  const binary = atob(value);
  const bytes = new Uint8Array(new ArrayBuffer(binary.length));
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

const keyStore = "factorflow.document-keys";

/**
 * Keys are kept in this browser and nowhere else.
 *
 * That is the honest consequence of the design rather than a shortcut: a key the platform
 * could recover is a key the platform could use. With a confidential workflow in front of it
 * the key would instead be wrapped to the enclave's public key, which is the one party that
 * is supposed to read a document — and still not this server.
 */
export function rememberKey(invoiceID: string, key: string): void {
  try {
    const kept = JSON.parse(localStorage.getItem(keyStore) ?? "{}") as Record<string, string>;
    kept[invoiceID] = key;
    localStorage.setItem(keyStore, JSON.stringify(kept));
  } catch {
    // A browser refusing storage costs the ability to reopen the document later, and must
    // not cost the upload itself.
  }
}

export function recallKey(invoiceID: string): string | null {
  try {
    const kept = JSON.parse(localStorage.getItem(keyStore) ?? "{}") as Record<string, string>;
    return kept[invoiceID] ?? null;
  } catch {
    return null;
  }
}
