import { afterEach, describe, expect, it, vi } from "vitest";
import {
  assertLaunchTarget,
  launchInNewTab,
  sessionFrameName,
  sessionLabel,
  sessionFrameUrl,
  sessionOrigin,
  sessionPath,
  submitLaunch,
  sessionFrameAllow,
  SESSION_FRAME_SANDBOX,
  TICKET_FIELD,
  type LaunchTicket,
} from "../../../src/session/launch";

const WS = "ws_0123456789abcdef";
const DOMAIN = "session.example.com";
const ORIGIN = "https://ws-0123456789abcdef.session.example.com";

function ticket(launchUrl: string): LaunchTicket {
  return { workspaceId: WS, ticket: "tkt_test", launchUrl, expiresAt: "2026-10-02T00:01:00Z" };
}

function watchFormSubmits() {
  const submitted: HTMLFormElement[] = [];
  const spy = vi
    .spyOn(HTMLFormElement.prototype, "submit")
    .mockImplementation(function (this: HTMLFormElement) {
      submitted.push(this);
    });
  return { submitted, spy };
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("session host mapping", () => {
  it("sessionOrigin", () => {
    expect(sessionOrigin("ws_0123456789abcdef", "session.example.com")).toBe(
      "https://ws-0123456789abcdef.session.example.com",
    );
  });

  it("sessionOrigin keeps a configured port", () => {
    expect(sessionOrigin(WS, "session.example.com:8443")).toBe(
      "https://ws-0123456789abcdef.session.example.com:8443",
    );
  });

  // FX-R18 + V3.24 embedded parity: KasmVNC forces resize=off, webp off and a
  // 20-minute client idle cut when it runs inside an iframe; the portal
  // re-asserts tab-mode settings on every frame navigation, and the
  // clipboard client flags follow the workspace policy.
  it("sessionFrameUrl carries the embedded-mode settings", () => {
    const base =
      "resize=remote&enable_webp=true&idle_disconnect=1440&clipboard_up=false&clipboard_down=false";
    expect(sessionFrameUrl(WS, "session.example.com")).toBe(
      `https://ws-0123456789abcdef.session.example.com/?${base}`,
    );
    expect(sessionFrameUrl(WS, "session.example.com:8443")).toBe(
      `https://ws-0123456789abcdef.session.example.com:8443/?${base}`,
    );
    // show_control_bar stays out: the portal owns session chrome.
    expect(sessionFrameUrl(WS, "session.example.com")).not.toContain("control_bar");
  });

  it("sessionFrameUrl sets the clipboard client flags per workspace policy", () => {
    const url = (policy?: string) =>
      new URL(
        sessionFrameUrl(WS, "session.example.com", {
          clipboardPolicy: policy as "Disabled" | "Send" | "Receive" | "Bidirectional" | undefined,
        }),
      );
    expect(url().searchParams.get("clipboard_up")).toBe("false");
    expect(url().searchParams.get("clipboard_down")).toBe("false");
    expect(url("Disabled").searchParams.get("clipboard_up")).toBe("false");
    expect(url("Disabled").searchParams.get("clipboard_down")).toBe("false");
    expect(url("Send").searchParams.get("clipboard_up")).toBe("true");
    expect(url("Send").searchParams.get("clipboard_down")).toBe("false");
    expect(url("Receive").searchParams.get("clipboard_up")).toBe("false");
    expect(url("Receive").searchParams.get("clipboard_down")).toBe("true");
    expect(url("Bidirectional").searchParams.get("clipboard_up")).toBe("true");
    expect(url("Bidirectional").searchParams.get("clipboard_down")).toBe("true");
    // Seamless paste is enabled only where the client would enable it
    // itself: jsdom's UA is Chrome-family, so an enabled policy gets it.
    expect(url("Bidirectional").searchParams.get("clipboard_seamless")).toBe("true");
    expect(url("Disabled").searchParams.get("clipboard_seamless")).toBeNull();
  });

  it("sessionLabel maps underscores to dashes", () => {
    expect(sessionLabel("ws_ab")).toBe("ws-ab");
    expect(sessionLabel(WS)).toBe("ws-0123456789abcdef");
  });
});

describe("assertLaunchTarget (SEC-26, per-workspace hosts)", () => {
  it("accepts the workspace's own session host", () => {
    expect(assertLaunchTarget(ticket(`${ORIGIN}/v1/launch`), WS, DOMAIN)).toBe(ORIGIN);
  });

  it("rejects another workspace's host", () => {
    const t = ticket("https://ws-otherworkspace.session.example.com/v1/launch");
    expect(() => assertLaunchTarget(t, WS, DOMAIN)).toThrow();
  });

  it("rejects a foreign domain", () => {
    const t = ticket("https://evil.example/v1/launch");
    expect(() => assertLaunchTarget(t, WS, DOMAIN)).toThrow();
  });

  it("rejects a subdomain below the workspace host", () => {
    const t = ticket(`https://a.${ORIGIN.slice("https://".length)}/v1/launch`);
    expect(() => assertLaunchTarget(t, WS, DOMAIN)).toThrow();
  });

  it("rejects a malformed launchUrl", () => {
    expect(() => assertLaunchTarget(ticket("not a url"), WS, DOMAIN)).toThrow();
  });

  it("rejects when no session domain is configured", () => {
    expect(() => assertLaunchTarget(ticket(`${ORIGIN}/v1/launch`), WS, "")).toThrow();
  });
});

describe("submitLaunch", () => {
  it("POSTs the ticket in a form body targeting the named frame", () => {
    const { submitted } = watchFormSubmits();
    const t = ticket(`${ORIGIN}/v1/launch`);
    const frame = sessionFrameName(WS);

    submitLaunch(t, frame, WS, DOMAIN);

    expect(submitted).toHaveLength(1);
    const form = submitted[0];
    expect(form.method).toBe("post");
    expect(form.action).toBe(`${ORIGIN}/v1/launch`);
    expect(form.target).toBe(frame);
    const input = form.querySelector(`input[name="${TICKET_FIELD}"]`) as HTMLInputElement;
    expect(input.value).toBe("tkt_test");
    // The ticket must never reach a URL, history entry or web storage.
    expect(form.action).not.toContain(input.value);
    expect(window.location.href).not.toContain(input.value);
    expect(JSON.stringify(localStorage)).not.toContain(input.value);
    expect(document.body.contains(form)).toBe(false); // removed after submit
  });

  it("refuses to submit to a foreign launchUrl", () => {
    const { submitted } = watchFormSubmits();
    expect(() =>
      submitLaunch(ticket("https://evil.example/v1/launch"), sessionFrameName(WS), WS, DOMAIN),
    ).toThrow();
    expect(submitted).toHaveLength(0);
  });

  it("launchInNewTab targets _blank with rel=noopener", () => {
    const { submitted } = watchFormSubmits();
    launchInNewTab(ticket(`${ORIGIN}/v1/launch`), WS, DOMAIN);
    expect(submitted).toHaveLength(1);
    expect(submitted[0].target).toBe("_blank");
    expect(submitted[0].rel).toBe("noopener");
  });
});

describe("routes and frame contract", () => {
  it("sessionPath is the in-portal route", () => {
    expect(sessionPath(WS)).toBe(`/workspaces/${WS}/session`);
  });

  it("frame sandbox and permissions match D13 exactly", () => {
    expect(SESSION_FRAME_SANDBOX).toBe(
      "allow-scripts allow-same-origin allow-forms allow-pointer-lock",
    );
    for (const forbidden of ["allow-top-navigation", "allow-popups", "allow-modals"]) {
      expect(SESSION_FRAME_SANDBOX.split(" ")).not.toContain(forbidden);
    }
    // keyboard-map (FX-R22, kept by the V3.24 layout check): lets the desktop
    // client's getLayoutMap() map non-US layouts — without it a German
    // Ctrl+Z reaches the remote as Ctrl+Y. It only exposes the layout to the
    // session origin, which the gateway Permissions-Policy grants to self.
    // Every feature names the session origin: the frame has no src
    // attribute, so a bare feature would delegate to the portal's own
    // origin and nothing reaches the session. Clipboard features follow
    // the workspace's policy (least privilege): none until it is known.
    const origin = `https://ws-${WS.slice(3)}.session.example.com`;
    expect(sessionFrameAllow(WS, "session.example.com")).toBe(
      `fullscreen ${origin}; keyboard-map ${origin}`,
    );
    expect(sessionFrameAllow(WS, "session.example.com", { clipboardPolicy: "Bidirectional" })).toBe(
      `clipboard-read ${origin}; clipboard-write ${origin}; fullscreen ${origin}; keyboard-map ${origin}`,
    );
    expect(sessionFrameAllow(WS, "session.example.com", { clipboardPolicy: "Send" })).toBe(
      `clipboard-read ${origin}; fullscreen ${origin}; keyboard-map ${origin}`,
    );
    expect(sessionFrameAllow(WS, "session.example.com", { clipboardPolicy: "Receive" })).toBe(
      `clipboard-write ${origin}; fullscreen ${origin}; keyboard-map ${origin}`,
    );
    expect(sessionFrameAllow(WS, "session.example.com", { clipboardPolicy: "Disabled" })).toBe(
      `fullscreen ${origin}; keyboard-map ${origin}`,
    );
    expect(sessionFrameAllow(WS, "")).toBe("");
  });
});
