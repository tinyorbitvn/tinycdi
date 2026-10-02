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

  "auth.gate.apiUnreachable": "Could not reach the workspace API: {error}",
  "auth.gate.checking": "Checking session…",

  "nav.sections": "Sections",
  "nav.workspaces": "Workspaces",
  "nav.admin": "Admin",
  "nav.newWorkspace": "New workspace",
  "nav.templates": "Template catalog",
  "nav.data": "Retained data",
  "nav.allWorkspaces": "← All workspaces",
} as const;
