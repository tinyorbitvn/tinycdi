import { afterEach, describe, expect, it, vi as vitest } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { LocaleProvider, useLocale } from "../../../src/app/locale";
import {
  detectLocale,
  getLocale,
  LOCALE_STORAGE_KEY,
  setActiveLocale,
  t,
} from "../../../src/i18n";

// Language switch (E11): sets <html lang>, swaps the catalog, and persists
// under tcdi.lang — a reload re-detects the stored choice.

function Probe() {
  const { locale, setLocale } = useLocale();
  return (
    <div>
      <output data-testid="locale">{locale}</output>
      <output data-testid="text">{t("nav.admin")}</output>
      <button onClick={() => setLocale("vi")}>switch</button>
    </div>
  );
}

afterEach(() => {
  setActiveLocale("en");
  vitest.restoreAllMocks();
});

describe("locale detection", () => {
  it("defaults to English", () => {
    expect(detectLocale()).toBe("en");
  });

  it("follows navigator.language starting with vi", () => {
    vitest
      .spyOn(window.navigator, "language", "get")
      .mockReturnValue("vi-VN");
    expect(detectLocale()).toBe("vi");
  });

  it("stored preference wins over navigator.language", () => {
    window.localStorage.setItem(LOCALE_STORAGE_KEY, "en");
    vitest
      .spyOn(window.navigator, "language", "get")
      .mockReturnValue("vi-VN");
    expect(detectLocale()).toBe("en");
  });
});

describe("language switch", () => {
  it("sets <html lang>, swaps t() output and survives a reload", () => {
    render(
      <LocaleProvider>
        <Probe />
      </LocaleProvider>,
    );
    expect(document.documentElement.lang).toBe("en");
    expect(screen.getByTestId("text")).toHaveTextContent("Admin");

    fireEvent.click(screen.getByRole("button", { name: "switch" }));

    expect(document.documentElement.lang).toBe("vi");
    expect(getLocale()).toBe("vi");
    expect(screen.getByTestId("text")).toHaveTextContent("Quản trị");
    expect(window.localStorage.getItem(LOCALE_STORAGE_KEY)).toBe("vi");
    // A reload re-runs detection: the stored choice is picked up again.
    expect(detectLocale()).toBe("vi");
  });
});
