// App-shell strings: router fallback, auth gate, navigation. Owned by T3.2
// after T3.3 seeds it; append-only.
export default {
  "app.notFound.title": "Page not found",
  "app.notFound.body": "The address {path} does not match any page in this portal.",
  "app.notFound.action": "Go to workspaces",
  "app.loadError.title": "This page could not be loaded",
  "app.loadError.body": "Reload the page to try again.",

  "app.shell.byline": "by TinyOrbit",
  "app.theme.label": "Theme",
  "app.theme.light": "Light",
  "app.theme.dark": "Dark",
  "app.theme.system": "System",

  "app.user.menu": "Account menu for {name}",
  "app.user.signedInAs": "Signed in as {name}",
  "app.user.tenant": "Tenant {tenant}",
  "app.user.signOut": "Sign out",
  "app.user.signOutFailed.title": "Could not sign out",
  "app.user.signOutFailed.body": "Try again. If it keeps failing, close this browser window.",

  "auth.signedOut.title": "You have signed out",
  "auth.signedOut.body": "Your session has ended. Close this window or sign in again.",
  "auth.signedOut.action": "Sign in again",

  "auth.gate.apiUnreachable": "Could not reach the workspace API: {error}",
  "auth.gate.checking": "Checking session…",
  "auth.gate.retrying": "Could not reach the workspace API ({error}). Retrying…",

  "nav.sections": "Sections",
  "nav.workspaces": "Workspaces",
  "nav.admin": "Admin",
  "nav.newWorkspace": "New workspace",
  "nav.templates": "Template catalog",
  "nav.data": "Retained data",
  "nav.allWorkspaces": "All workspaces",
} as const;
