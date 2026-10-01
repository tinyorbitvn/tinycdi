// English catalog — assembled only by spreading the per-area files under
// ./en so each screen task edits its own file (AMENDMENTS-1 item 5). Keys are
// area.screen.element; {name} placeholders are substituted by t() in
// ./index. English-only in v0.2 (D33); append-only — never rename a key.
import common from "./en/common";
import app from "./en/app";
import workspaces from "./en/workspaces";
import session from "./en/session";
import admin from "./en/admin";
import data from "./en/data";

export const en = {
  ...common,
  ...app,
  ...workspaces,
  ...session,
  ...admin,
  ...data,
} as const;
