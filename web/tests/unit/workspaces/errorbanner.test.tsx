import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { PortalApiError } from "../../../src/api/errors";
import { ErrorBanner } from "../../../src/workspaces/ErrorBanner";

const quota = () =>
  new PortalApiError(409, {
    code: "QUOTA_EXHAUSTED",
    message: "tenant quota has no headroom",
    retryable: false,
    requestId: "req_42",
  });

describe("ErrorBanner", () => {
  it("titles the alert with the guidance, not the raw code", () => {
    render(<ErrorBanner error={quota()} />);
    const title = screen.getByRole("alert").querySelector(".tc-alert__title")!;
    expect(title).toHaveTextContent(
      "Quota exhausted — delete an unused workspace or ask an administrator for more quota.",
    );
    expect(title).not.toHaveTextContent("QUOTA_EXHAUSTED");
  });

  it("shows the server message, then code and request id on a muted line", () => {
    render(<ErrorBanner error={quota()} />);
    const alert = screen.getByRole("alert");
    expect(alert.querySelector(".tc-alert__body")).toHaveTextContent("tenant quota has no headroom");
    expect(alert.querySelector(".tc-alert__meta")).toHaveTextContent("QUOTA_EXHAUSTED · Request ID: req_42");
  });

  it("dismisses through the icon button", () => {
    const onDismiss = vi.fn();
    render(<ErrorBanner error={quota()} onDismiss={onDismiss} />);
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(onDismiss).toHaveBeenCalledOnce();
  });

  it("a non-API error keeps the fallback title and its message, without a meta line", () => {
    render(<ErrorBanner error={new Error("network down")} />);
    const alert = screen.getByRole("alert");
    expect(alert.querySelector(".tc-alert__title")).toHaveTextContent("Something went wrong");
    expect(alert).toHaveTextContent("network down");
    expect(alert.querySelector(".tc-alert__meta")).toBeNull();
  });
});
