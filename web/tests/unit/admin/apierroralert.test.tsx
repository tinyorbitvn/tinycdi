import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { PortalApiError } from "../../../src/api/errors";
import { ApiErrorAlert } from "../../../src/admin/ApiErrorAlert";

describe("ApiErrorAlert", () => {
  it("titles with the admin guidance and keeps the code in the muted meta line", () => {
    render(
      <ApiErrorAlert
        error={new PortalApiError(404, { code: "NOT_FOUND", message: "workspace ws_1 not found", requestId: "req_7" })}
      />,
    );
    const alert = screen.getByRole("alert");
    expect(alert.querySelector(".tc-alert__title")).toHaveTextContent(
      "It no longer exists or is not visible to you. Refresh the list.",
    );
    expect(alert.querySelector(".tc-alert__body")).toHaveTextContent("workspace ws_1 not found");
    expect(alert.querySelector(".tc-alert__meta")).toHaveTextContent("NOT_FOUND · Request ID: req_7");
  });

  it("an explicit title keeps the guidance in the body", () => {
    render(<ApiErrorAlert title="Save failed" error={new PortalApiError(404, { code: "NOT_FOUND" })} />);
    const alert = screen.getByRole("alert");
    expect(alert.querySelector(".tc-alert__title")).toHaveTextContent("Save failed");
    expect(alert.querySelector(".tc-alert__body")).toHaveTextContent("It no longer exists");
  });
});
