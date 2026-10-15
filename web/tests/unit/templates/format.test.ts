import { describe, expect, it } from "vitest";
import { clipboardPolicyLabel } from "../../../src/templates/format";

// The API sends the four values of the WorkspaceTemplate CRD enum; each has
// its own label.
describe("clipboardPolicyLabel", () => {
  it.each([
    ["Disabled", "Disabled"],
    ["Send", "Send only"],
    ["Receive", "Receive only"],
    ["Bidirectional", "Both directions"],
  ] as const)("%s -> %s", (policy, label) => {
    expect(clipboardPolicyLabel(policy)).toBe(label);
  });

  it("never reports a permissive policy as Disabled", () => {
    for (const policy of ["Send", "Receive", "Bidirectional"] as const) {
      expect(clipboardPolicyLabel(policy)).not.toBe("Disabled");
    }
  });
});
