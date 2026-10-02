import { AuthGate } from "./auth/AuthGate";
import { SignedOut } from "./auth/SignedOut";
import { SIGNED_OUT_PATH } from "./auth/signOut";
import { usePathname } from "./app/router";
import { AppShell, BrandingProvider } from "./app/shell";
import { MeProvider } from "./app/me";
import { ThemeProvider } from "./app/theme";
import { ToastProvider } from "./design";

export function App() {
  const pathname = usePathname();
  // The signed-out page is public: outside AuthGate, so it neither probes
  // the session nor starts a login.
  const signedOut = pathname.replace(/\/+$/, "") === SIGNED_OUT_PATH;
  return (
    <ThemeProvider>
      <BrandingProvider>
        <ToastProvider>
          {signedOut ? (
            <SignedOut />
          ) : (
            <AuthGate>
              <MeProvider>
                <AppShell />
              </MeProvider>
            </AuthGate>
          )}
        </ToastProvider>
      </BrandingProvider>
    </ThemeProvider>
  );
}
