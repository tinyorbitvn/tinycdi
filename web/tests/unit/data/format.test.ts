import { describe, expect, it } from "vitest";
import { shortId } from "../../../src/data/format";

describe("shortId", () => {
  it("keeps the prefix + 4 and the last 2 characters", () => {
    expect(shortId("rd_01J4Z9W2PFK8G4TQ3M7H1R5N0A")).toBe("rd_01J4…0A");
  });

  it("leaves ids of 10 characters or fewer alone", () => {
    expect(shortId("rd_123")).toBe("rd_123");
    expect(shortId("rd_1234567")).toBe("rd_1234567");
    expect(shortId("")).toBe("");
  });

  it("bounds a pathological id to 10 characters", () => {
    expect(shortId("x".repeat(200))).toHaveLength(10);
  });
});
