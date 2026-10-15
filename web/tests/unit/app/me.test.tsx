import { describe, expect, it } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import {
  MeHttpError,
  MeProvider,
  isTransientMeError,
  useMe,
  type Me,
} from "../../../src/app/me";

// The second /v1/me read (after AuthGate opens) must not dead-end the shell
// on a backend blip either (V3.27): transient failures stay "loading" and
// retry with backoff; a hard failure reports error once.

const ME: Me = {
  subject: "user-x",
  displayName: "Ada",
  tenant: "acme",
  roles: ["user"],
};

function Probe() {
  const s = useMe();
  return <p>{s.status === "ready" ? `hello ${s.me.displayName}` : s.status}</p>;
}

describe("MeProvider", () => {
  it("isTransientMeError: 5xx/429/network transient, 4xx not", () => {
    expect(isTransientMeError(new MeHttpError(503))).toBe(true);
    expect(isTransientMeError(new MeHttpError(429))).toBe(true);
    expect(isTransientMeError(new TypeError("fetch failed"))).toBe(true);
    expect(isTransientMeError(new MeHttpError(403))).toBe(false);
    expect(isTransientMeError(new Error("boom"))).toBe(false);
  });

  it("a transient /v1/me failure retries until it recovers", async () => {
    let healthy = false;
    const load = async (): Promise<Me> => {
      if (!healthy) throw new MeHttpError(503);
      return ME;
    };
    render(
      <MeProvider load={load}>
        <Probe />
      </MeProvider>,
    );
    expect(screen.getByText("loading")).toBeInTheDocument();
    healthy = true;
    expect(await screen.findByText("hello Ada", {}, { timeout: 15_000 })).toBeInTheDocument();
  }, 20_000);

  it("a hard failure reports error without a retry loop", async () => {
    let calls = 0;
    const load = async (): Promise<Me> => {
      calls += 1;
      throw new MeHttpError(400);
    };
    render(
      <MeProvider load={load}>
        <Probe />
      </MeProvider>,
    );
    await waitFor(() => expect(screen.getByText("error")).toBeInTheDocument());
    await new Promise((r) => setTimeout(r, 300));
    expect(calls).toBe(1);
  });
});
