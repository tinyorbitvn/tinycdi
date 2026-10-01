import { describe, expect, test } from "vitest";
import { DEFAULT_BRANDING, loadBranding } from "../../../src/app/branding";

function respond(res: Response): typeof fetch {
  return (() => Promise.resolve(res)) as typeof fetch;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

describe("loadBranding", () => {
  test("branding: defaults on 404", async () => {
    const branding = await loadBranding(respond(new Response("not found", { status: 404 })));
    expect(branding).toEqual(DEFAULT_BRANDING);
  });

  test("branding: non-JSON response falls back", async () => {
    // Before the branding directory is configured the SPA fallback answers
    // /branding/branding.json with index.html.
    const html = new Response("<!doctype html><html></html>", {
      status: 200,
      headers: { "content-type": "text/html" },
    });
    await expect(loadBranding(respond(html))).resolves.toEqual(DEFAULT_BRANDING);
  });

  test("branding: rejects absolute logo URLs", async () => {
    const branding = await loadBranding(
      respond(
        json({
          productName: "Acme Desktops",
          logo: "https://x/y.svg",
          logoDark: "//cdn.example.com/d.svg",
        }),
      ),
    );
    expect(branding.productName).toBe("Acme Desktops");
    expect(branding.logo).toBe(DEFAULT_BRANDING.logo);
    expect(branding.logoDark).toBe(DEFAULT_BRANDING.logo);
  });

  test("accepts logos under /branding/ only", async () => {
    const branding = await loadBranding(
      respond(
        json({
          productName: "Acme",
          logo: "/branding/logo.svg",
          logoDark: "logo-dark.svg",
        }),
      ),
    );
    expect(branding.logo).toBe("/branding/logo.svg");
    expect(branding.logoDark).toBe("/branding/logo.svg");
  });

  test("network error falls back", async () => {
    const fail = (() => Promise.reject(new Error("offline"))) as typeof fetch;
    await expect(loadBranding(fail)).resolves.toEqual(DEFAULT_BRANDING);
  });

  test("invalid JSON body falls back", async () => {
    const res = new Response("{nope", {
      status: 200,
      headers: { "content-type": "application/json" },
    });
    await expect(loadBranding(respond(res))).resolves.toEqual(DEFAULT_BRANDING);
  });

  test("default branding is TinyCDI", () => {
    expect(DEFAULT_BRANDING.productName).toBe("TinyCDI");
    expect(DEFAULT_BRANDING.logo).toBeTruthy();
  });
});
