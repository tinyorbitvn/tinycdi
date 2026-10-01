import type { AppRoute } from "../app/route-types";
import { SessionPage } from "./SessionPage";

// The in-portal session view: full-height ("bleed") under the shell's top
// bar, so the desktop gets every pixel the portal can spare.
export const routes: AppRoute[] = [
  {
    path: "/workspaces/:id/session",
    title: "Session",
    layout: "bleed",
    render: (p) => <SessionPage key={p.id} workspaceId={p.id!} />,
  },
];
