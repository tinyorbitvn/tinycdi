import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, test } from "vitest";
import { ToastProvider } from "../../../src/design";
import { ApiProvider } from "../../../src/api/context";
import { AppShell, BrandingProvider } from "../../../src/app/shell";
import { MeProvider, type Me } from "../../../src/app/me";
import { ThemeProvider, THEME_STORAGE_KEY } from "../../../src/app/theme";
import { LocaleProvider } from "../../../src/app/locale";
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
          <LocaleProvider>
            <ToastProvider>
              <MeProvider load={async () => TEST_ME}>
                <AppShell areas={TEST_AREAS} />
              </MeProvider>
            </ToastProvider>
          </LocaleProvider>
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

  test("skip link is first and targets main content", async () => {
    renderShell();
    expect(await screen.findByText("TinyCDI")).toBeInTheDocument();
    const skip = screen.getByRole("link", { name: "Skip to content" });
    expect(skip).toHaveAttribute("href", "#tc-main");
    const main = document.getElementById("tc-main");
    expect(main?.tagName).toBe("MAIN");
    expect(main).toHaveAttribute("tabindex", "-1");
    // The skip link precedes the top bar in focus order.
    const firstLink = document.body.querySelector(".tc-shell > *:first-child");
    expect(firstLink).toBe(skip);
  });

  test("menu button opens the nav drawer; Escape closes and restores focus", async () => {
    renderShell();
    await screen.findByText("TinyCDI");
    const toggle = screen.getByRole("button", { name: "Menu" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    // jsdom does not focus buttons on click like a real browser does;
    // focus it first so the trap's restore target is meaningful.
    toggle.focus();
    fireEvent.click(toggle);
    const dialog = await screen.findByRole("dialog", { name: "Sections" });
    expect(within(dialog).getByRole("navigation", { name: "Sections" })).toBeInTheDocument();
    expect(within(dialog).getByRole("link", { name: "Workspaces" })).toBeInTheDocument();
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(toggle).toHaveFocus();
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
            <LocaleProvider>
              <ToastProvider>
                <MeProvider load={async () => ({ ...TEST_ME, roles: ["tenant-admin"] })}>
                  <AppShell areas={TEST_AREAS} />
                </MeProvider>
              </ToastProvider>
            </LocaleProvider>
          </BrandingProvider>
        </ThemeProvider>
      </ApiProvider>,
    );
    expect(await screen.findByRole("link", { name: "Admin" })).toBeInTheDocument();
  });
});
