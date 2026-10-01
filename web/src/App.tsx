import { AuthGate } from "./auth/AuthGate";
import { AppShell, BrandingProvider } from "./app/shell";
import { MeProvider } from "./app/me";
import { ThemeProvider } from "./app/theme";
import { ToastProvider } from "./design";

export function App() {
  return (
    <ThemeProvider>
      <BrandingProvider>
        <ToastProvider>
          <AuthGate>
            <MeProvider>
              <AppShell />
            </MeProvider>
          </AuthGate>
        </ToastProvider>
      </BrandingProvider>
    </ThemeProvider>
  );
}
