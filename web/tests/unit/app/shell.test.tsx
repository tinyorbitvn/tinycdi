import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, test } from "vitest";
import { ToastProvider } from "../../../src/design";
import { ApiProvider } from "../../../src/api/context";
import { AppShell, BrandingProvider } from "../../../src/app/shell";
import { MeProvider, type Me } from "../../../src/app/me";
import { ThemeProvider, THEME_STORAGE_KEY } from "../../../src/app/theme";
import { DEFAULT_BRANDING, type Branding } from "../../../src/app/branding";
import type { RouteArea } from "../../../src/app/routes";

const TEST_AREAS: RouteArea[] = [
  {
    name: "workspaces",
    owns: () => true,
    load: async () => ({
      routes: [{ path: "/*", title: "Home", render: () => <div>Page content</div> }],
    }),
  },
];

const TEST_ME: Me = {
  subject: "u1",
  displayName: "Ada Admin",
  tenant: "acme",
  roles: [],
};

function renderShell(branding: Branding = DEFAULT_BRANDING) {
  return render(
    <ApiProvider>
      <ThemeProvider>
        <BrandingProvider load={async () => branding}>
          <ToastProvider>
            <MeProvider load={async () => TEST_ME}>
              <AppShell areas={TEST_AREAS} />
            </MeProvider>
          </ToastProvider>
        </BrandingProvider>
      </ThemeProvider>
    </ApiProvider>,
  );
}

describe("AppShell", () => {
  beforeEach(() => {
    document.documentElement.removeAttribute("data-theme");
  });

  test("shell: shows product name", async () => {
    renderShell();
    expect(await screen.findByText("TinyCDI")).toBeInTheDocument();
    expect(screen.getByText("by TinyOrbit")).toBeInTheDocument();
  });

  test("shell: custom branding drops the TinyOrbit byline", async () => {
    renderShell({ productName: "Acme Desktops", logo: "/branding/logo.svg", logoDark: null });
    expect(await screen.findByText("Acme Desktops")).toBeInTheDocument();
    expect(screen.queryByText("TinyCDI")).not.toBeInTheDocument();
    expect(screen.queryByText("by TinyOrbit")).not.toBeInTheDocument();
    const logo = document.querySelector(".tc-topbar__mark");
    expect(logo).toHaveAttribute("src", "/branding/logo.svg");
  });

  test("theme: toggle sets data-theme", async () => {
    renderShell();
    fireEvent.click(await screen.findByRole("button", { name: "Theme" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Dark" }));
    expect(document.documentElement).toHaveAttribute("data-theme", "dark");
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("dark");
  });

  test("nav hides Admin without the tenant-admin role", async () => {
    renderShell();
    await screen.findByText("Workspaces");
    expect(screen.queryByRole("link", { name: "Admin" })).not.toBeInTheDocument();
  });

  test("nav shows Admin for tenant admins", async () => {
    render(
      <ApiProvider>
        <ThemeProvider>
          <BrandingProvider load={async () => DEFAULT_BRANDING}>
            <ToastProvider>
              <MeProvider load={async () => ({ ...TEST_ME, roles: ["tenant-admin"] })}>
                <AppShell areas={TEST_AREAS} />
              </MeProvider>
            </ToastProvider>
          </BrandingProvider>
        </ThemeProvider>
      </ApiProvider>,
    );
    expect(await screen.findByRole("link", { name: "Admin" })).toBeInTheDocument();
  });
});
