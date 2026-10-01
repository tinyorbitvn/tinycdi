// App-shell strings: router fallback, auth gate, navigation. Owned by T3.2
// after T3.3 seeds it; append-only.
export default {
  "app.notFound.title": "Not found",
  "app.notFound.body": "No page for {path}.",

  "auth.gate.apiUnreachable": "Could not reach the workspace API: {error}",
  "auth.gate.checking": "Checking session…",

  "nav.newWorkspace": "New workspace",
  "nav.templates": "Template catalog",
  "nav.data": "Retained data",
  "nav.allWorkspaces": "← All workspaces",
} as const;
