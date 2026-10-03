import { useEffect } from "react";
import { buttonClass, EmptyState, IconLogOut } from "../design";
import { t } from "../i18n";
import { SIGN_IN_AGAIN_URL } from "./signOut";

// Public page after sign-out. It makes no API call and starts no login on
// load — the identity provider session may still be alive, and logging in
// automatically would sign the user straight back in. "Sign in again" is a
// plain link, so the login is always the user's own action.
export function SignedOut() {
  useEffect(() => {
    document.title = t("auth.signedOut.title");
  }, []);
  return (
    <main className="tc-signedout">
      <EmptyState
        icon={<IconLogOut size={32} />}
        headingLevel={2}
        title={t("auth.signedOut.title")}
        description={t("auth.signedOut.body")}
        action={
          <a className={buttonClass("primary")} href={SIGN_IN_AGAIN_URL}>
            {t("auth.signedOut.action")}
          </a>
        }
      />
    </main>
  );
}
