import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "../api/client";
import { digest, fromBase64, open, recallKey } from "../crypto";
import { SessionProvider, participants } from "../session";
import { NewInvoice } from "./NewInvoice";

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    api: {
      me: vi.fn(),
      organization: vi.fn(),
      createInvoice: vi.fn(),
      uploadDocument: vi.fn(),
      requestAssessment: vi.fn(),
    },
  };
});

const issuer = participants[0]!;
const plaintext = "INVOICE INV-2026-0007\nACME Logistics GmbH\n15000.00 USD";

function signedIn() {
  vi.mocked(api.me).mockResolvedValue({
    organization_id: issuer.id,
    wallet: "0x00000000000000000000000000000000000000a1",
    role: "owner",
    eligible: true,
    operator: false,
  });
  vi.mocked(api.organization).mockResolvedValue({
    id: issuer.id,
    type: "ISSUER",
    name: issuer.name,
    wallet: "0x00000000000000000000000000000000000000a1",
    eligibility: "ELIGIBLE",
    can_issue: true,
    can_invest: false,
    version: 1,
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
  });
}

function renderUpload() {
  signedIn();
  return render(
    <MemoryRouter>
      <SessionProvider>
        <NewInvoice />
      </SessionProvider>
    </MemoryRouter>,
  );
}

async function fillAndSubmit() {
  await userEvent.type(await screen.findByLabelText("Invoice number"), "INV-2026-0007");
  await userEvent.type(screen.getByLabelText("Debtor"), "ACME Logistics GmbH");
  await userEvent.type(screen.getByLabelText("Face value"), "15000.00");

  const file = new File([plaintext], "invoice.pdf", { type: "application/pdf" });
  await userEvent.upload(screen.getByLabelText("Document (PDF)"), file);
  await userEvent.click(screen.getByRole("button", { name: "Encrypt and upload" }));
}

describe("uploading a receivable", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();

    vi.mocked(api.createInvoice).mockResolvedValue({
      id: "invoice-1",
      issuer_id: issuer.id,
      debtor_ref: "ACME Logistics GmbH",
      number: "INV-2026-0007",
      face: "15000.00",
      currency: "USD",
      issued_at: "2026-09-07T00:00:00Z",
      due_at: "2026-11-07T00:00:00Z",
      tenor_days: 61,
      status: "DRAFT",
      version: 1,
      created_at: "2026-09-08T00:00:00Z",
      updated_at: "2026-09-08T00:00:00Z",
    });
    vi.mocked(api.requestAssessment).mockResolvedValue({} as never);
  });

  /*
   * The one property worth testing here: what leaves the browser is ciphertext. The document
   * is recovered from what was sent, using the key that stayed behind — which proves both
   * that the upload is real and that the key never needed to travel with it.
   */
  it("sends ciphertext the platform cannot read, and keeps the key here", async () => {
    let sent = "";
    vi.mocked(api.uploadDocument).mockImplementation(async (_auth, _id, document) => {
      sent = document.ciphertext;
      return {
        cipher_hash: await digest(fromBase64(document.ciphertext)),
        size_bytes: fromBase64(document.ciphertext).length,
        status: "UPLOADED",
      };
    });

    renderUpload();
    await fillAndSubmit();

    await waitFor(() => expect(api.uploadDocument).toHaveBeenCalled());

    const ciphertext = fromBase64(sent);
    expect(new TextDecoder().decode(ciphertext)).not.toContain("ACME");

    const key = recallKey("invoice-1");
    expect(key).not.toBeNull();
    expect(new TextDecoder().decode(await open(ciphertext, key!))).toBe(plaintext);

    // The key is never an argument to anything the client sends.
    const [, , document] = vi.mocked(api.uploadDocument).mock.calls[0]!;
    expect(document.key_ref).toBe("browser-local");
    expect(JSON.stringify(document)).not.toContain(key!);
  });

  /** The invoice is created, the ciphertext uploaded, and only then is scoring queued. */
  it("queues the assessment after the document is stored", async () => {
    vi.mocked(api.uploadDocument).mockImplementation(async (_auth, _id, document) => ({
      cipher_hash: await digest(fromBase64(document.ciphertext)),
      size_bytes: 1,
      status: "UPLOADED",
    }));

    renderUpload();
    await fillAndSubmit();

    await waitFor(() => expect(api.requestAssessment).toHaveBeenCalledWith("", "invoice-1"));

    const created = vi.mocked(api.createInvoice).mock.calls[0]![1];
    expect(created.number).toBe("INV-2026-0007");
    expect(created.face).toBe("15000.00");
    expect(created.currency).toBe("USD");
  });

  /*
   * The digest is the only thing binding a price to a document, so a server that reports a
   * different one has stored something else — and that has to stop the flow rather than be
   * rounded off as a mismatch nobody reads.
   */
  it("stops when the server reports a different digest", async () => {
    vi.mocked(api.uploadDocument).mockResolvedValue({
      cipher_hash: "0".repeat(64),
      size_bytes: 1,
      status: "UPLOADED",
    });

    renderUpload();
    await fillAndSubmit();

    expect(await screen.findByRole("alert")).toHaveTextContent(/different digest/i);
    expect(api.requestAssessment).not.toHaveBeenCalled();
  });

  it("asks for a document before sending anything", async () => {
    renderUpload();

    await userEvent.type(await screen.findByLabelText("Invoice number"), "INV-2026-0007");
    await userEvent.click(screen.getByRole("button", { name: "Encrypt and upload" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(/Choose the invoice document/);
    expect(api.createInvoice).not.toHaveBeenCalled();
  });

  /** A refusal from the server is shown in its own words, and nothing is queued after it. */
  it("shows why the server refused", async () => {
    vi.mocked(api.createInvoice).mockRejectedValue(
      new Error("validation failed: face must be greater than zero"),
    );

    renderUpload();
    await fillAndSubmit();

    expect(await screen.findByRole("alert")).toHaveTextContent(/face must be greater than zero/);
    expect(api.uploadDocument).not.toHaveBeenCalled();
  });
});
