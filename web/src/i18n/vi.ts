// Vietnamese catalog — assembled exactly like ./en from the per-area files
// under ./vi (V3.14). Key parity with en is enforced by the satisfies check
// in each area file plus the catalog test and lint:strings.
import common from "./vi/common";
import app from "./vi/app";
import workspaces from "./vi/workspaces";
import session from "./vi/session";
import admin from "./vi/admin";
import data from "./vi/data";
import progress from "./vi/progress";

export const vi = {
  ...common,
  ...app,
  ...workspaces,
  ...session,
  ...admin,
  ...data,
  ...progress,
};
