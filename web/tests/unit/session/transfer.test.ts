import { describe, expect, it, vi } from "vitest";
import {
  TransferError,
  checkedContentUrl,
  getTransferContent,
  putTransferContent,
  transferDirections,
  validTransferName,
} from "../../../src/session/transfer";

const WS = "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E";
const DOMAIN = "session.example.test";
const ORIGIN = "https://ws-01j4z8kq2m9xnbv3t7yh0r6d5e.session.example.test";
const ID = "xf_abcdefghijklmnopqrstuvwxyz";

describe("transferDirections", () => {
  it("maps each policy", () => {
    expect(transferDirections("Disabled")).toEqual({ upload: false, download: false });
    expect(transferDirections("Upload")).toEqual({ upload: true, download: false });
    expect(transferDirections("Download")).toEqual({ upload: false, download: true });
    expect(transferDirections("Bidirectional")).toEqual({ upload: true, download: true });
    expect(transferDirections(undefined)).toEqual({ upload: false, download: false });
  });
});

describe("validTransferName", () => {
  it("accepts single file names and refuses the rest", () => {
    for (const ok of ["report.pdf", "Ảnh chụp.png", ".hidden", "a b.txt"]) expect(validTransferName(ok)).toBe(true);
    for (const bad of ["", ".", "..", "a/b", "a\\b", "x\u0000", "x\n", "é".repeat(128)]) expect(validTransferName(bad)).toBe(false);
  });
});

describe("checkedContentUrl", () => {
  it("accepts only the workspace's own session origin and route", () => {
    expect(checkedContentUrl(`${ORIGIN}/v1/transfers/${ID}/content`, WS, DOMAIN)).toBe(`${ORIGIN}/v1/transfers/${ID}/content`);
    for (const bad of [
      `https://evil.test/v1/transfers/${ID}/content`,
      `https://ws-other.session.example.test/v1/transfers/${ID}/content`,
      `${ORIGIN}/v1/launch`,
      `${ORIGIN}/v1/transfers/${ID}/content?x=1`,
      `http://ws-01j4z8kq2m9xnbv3t7yh0r6d5e.session.example.test/v1/transfers/${ID}/content`,
    ]) {
      expect(() => checkedContentUrl(bad, WS, DOMAIN)).toThrow(TransferError);
    }
  });
});

describe("content requests", () => {
  it("PUTs with credentials and returns the byte count", async () => {
    const f = vi.fn(async () => new Response(JSON.stringify({ bytes: 5 }), { status: 200 }));
    await expect(putTransferContent("https://h/v1/transfers/x/content", new Blob(["hello"]), f)).resolves.toBe(5);
    expect(f).toHaveBeenCalledWith("https://h/v1/transfers/x/content", expect.objectContaining({ method: "PUT", credentials: "include", mode: "cors" }));
  });
  it("surfaces the gateway error code", async () => {
    const f = vi.fn(async () => new Response(JSON.stringify({ error: "transfer_denied" }), { status: 403 }));
    await expect(getTransferContent("https://h/x", f)).rejects.toMatchObject({ code: "transfer_denied" });
    const g = vi.fn(async () => new Response("garbage", { status: 502 }));
    await expect(getTransferContent("https://h/x", g)).rejects.toMatchObject({ code: "upstream_failed" });
    const n = vi.fn(async () => {
      throw new TypeError("Failed to fetch");
    });
    await expect(getTransferContent("https://h/x", n)).rejects.toMatchObject({ code: "network" });
  });
});
