import { describe, expect, it } from "vitest";
import { en } from "../../../src/i18n/en";
import { vi } from "../../../src/i18n/vi";
import enCommon from "../../../src/i18n/en/common";
import enApp from "../../../src/i18n/en/app";
import enWorkspaces from "../../../src/i18n/en/workspaces";
import enSession from "../../../src/i18n/en/session";
import enAdmin from "../../../src/i18n/en/admin";
import enData from "../../../src/i18n/en/data";
import enProgress from "../../../src/i18n/en/progress";
import viCommon from "../../../src/i18n/vi/common";
import viApp from "../../../src/i18n/vi/app";
import viWorkspaces from "../../../src/i18n/vi/workspaces";
import viSession from "../../../src/i18n/vi/session";
import viAdmin from "../../../src/i18n/vi/admin";
import viData from "../../../src/i18n/vi/data";
import viProgress from "../../../src/i18n/vi/progress";

// Vietnamese catalog contract (E11): same key set as en per area file,
// same {placeholder} set per key, no emoji — the last also catches
// untranslated English sneaking in via a copy-paste.

const AREAS: [area: string, enArea: object, viArea: object][] = [
  ["common", enCommon, viCommon],
  ["app", enApp, viApp],
  ["workspaces", enWorkspaces, viWorkspaces],
  ["session", enSession, viSession],
  ["admin", enAdmin, viAdmin],
  ["data", enData, viData],
  ["progress", enProgress, viProgress],
];

const PLACEHOLDER = /\{([^{}]+)\}/g;
const paramsOf = (message: string): string[] =>
  [...message.matchAll(PLACEHOLDER)].map((m) => m[1]).sort();

describe("vi catalog", () => {
  it("key parity with en, per area file", () => {
    for (const [area, enArea, viArea] of AREAS) {
      expect(Object.keys(viArea).sort(), area).toEqual(
        Object.keys(enArea).sort(),
      );
    }
  });

  it("placeholders match en per key", () => {
    for (const key of Object.keys(en) as (keyof typeof en)[]) {
      expect(paramsOf(vi[key]), key).toEqual(paramsOf(en[key]));
    }
  });

  it("no emoji", () => {
    for (const [key, value] of Object.entries(vi)) {
      expect(value, key).not.toMatch(/\p{Extended_Pictographic}/u);
    }
  });

  // The in-use dialog covers the closed-tab release window, like en.
  it("in-use body tells the user a closed tab may still hold the workspace", () => {
    expect(vi["session.inUse.body"]).toContain("một tab bạn vừa đóng");
    expect(vi["session.inUse.body"]).toContain("Chờ khoảng một phút");
  });

  // A hint that explains option values must name them by the labels the
  // vi select actually shows, not the English enum names.
  it("session-policy clipboard hint names the vi option labels", () => {
    const hint = vi["admin.policy.clipboard.hint"];
    expect(hint).toContain(vi["templates.clipboard.send"]);
    expect(hint).toContain(vi["templates.clipboard.receive"]);
    expect(hint).not.toMatch(/\b(Send|Receive)\b/);
  });
});
