// File transfer between the browser and the workspace's Transfers folder
// (ADR 0009 §5). The portal mints a single-use grant on the API, then
// moves the bytes with a credentialed CORS request to the workspace's own
// session host; the gateway claims the grant and streams to the in-pod
// helper. A download lands in a Blob and is saved with <a download>
// so it works in both cookie modes.
import { sessionOrigin } from "./launch";

export type FileTransferPolicy = "Disabled" | "Upload" | "Download" | "Bidirectional";

export function transferDirections(p: FileTransferPolicy | undefined): { upload: boolean; download: boolean } {
  return {
    upload: p === "Upload" || p === "Bidirectional",
    download: p === "Download" || p === "Bidirectional",
  };
}

const encoder = new TextEncoder();

/** The backend's single-file-name rule (internal/api/filetransfers.go). */
export function validTransferName(name: string): boolean {
  const bytes = encoder.encode(name).length;
  if (bytes === 0 || bytes > 255 || name === "." || name === "..") return false;
  if (/[/\\;%]/.test(name) || name.includes("..")) return false;
  // eslint-disable-next-line no-control-regex
  return !/[\u0000-\u001f\u007f-\u009f]/.test(name);
}

export class TransferError extends Error {
  readonly code: string;
  constructor(code: string) {
    super(`file transfer failed: ${code}`);
    this.code = code;
  }
}

const CONTENT_PATH = /^\/v1\/transfers\/xf_[a-z2-7]{26}\/content$/;

/**
 * The content URL comes from the API; it is used only when it names this
 * workspace's own session origin and the transfer route, so a confused or
 * hostile response cannot point a credentialed request anywhere else.
 */
export function checkedContentUrl(contentUrl: string, workspaceId: string, sessionDomain: string): string {
  let u: URL;
  try {
    u = new URL(contentUrl);
  } catch {
    throw new TransferError("bad_url");
  }
  if (u.origin !== sessionOrigin(workspaceId, sessionDomain) || !CONTENT_PATH.test(u.pathname) || u.search || u.hash) {
    throw new TransferError("bad_url");
  }
  return u.href;
}

async function send(url: string, init: RequestInit, fetchImpl: typeof fetch): Promise<Response> {
  let res: Response;
  try {
    res = await fetchImpl(url, { ...init, credentials: "include", mode: "cors", cache: "no-store" });
  } catch {
    throw new TransferError("network");
  }
  if (!res.ok) {
    let code = "upstream_failed";
    try {
      const body = (await res.json()) as { error?: unknown };
      if (typeof body.error === "string") code = body.error;
    } catch {
      /* non-JSON error body */
    }
    throw new TransferError(code);
  }
  return res;
}

export async function putTransferContent(url: string, body: Blob, fetchImpl: typeof fetch = fetch): Promise<number> {
  const res = await send(url, { method: "PUT", body, headers: { "Content-Type": "application/octet-stream" } }, fetchImpl);
  const out = (await res.json()) as { bytes?: number };
  return typeof out.bytes === "number" ? out.bytes : body.size;
}

export async function getTransferContent(url: string, fetchImpl: typeof fetch = fetch): Promise<Blob> {
  const res = await send(url, { method: "GET" }, fetchImpl);
  return res.blob();
}

export function saveBlob(blob: Blob, name: string): void {
  const href = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = href;
  a.download = name;
  a.rel = "noopener";
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(href), 60_000);
}
