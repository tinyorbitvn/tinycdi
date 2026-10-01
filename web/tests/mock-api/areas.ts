// Registry of mock API areas composed by handler.ts. Each area lives in its
// own file (web/tests/mock-api/<area>.ts) and exports a MockAreaFactory
// (see core.ts). Order matters when two areas claim the same route: the
// first match wins — admin precedes workspaces so it can scope
// GET /v1/workspaces[/{id}] by owner.

import type { MockAreaFactory } from "./core.ts";
import { adminArea } from "./admin.ts";
import { authArea } from "./auth.ts";
import { dataArea } from "./data.ts";
import { sessionArea } from "./session.ts";
import { workspacesArea } from "./workspaces.ts";

export const AREAS: readonly MockAreaFactory[] = [authArea, sessionArea, adminArea, workspacesArea, dataArea];
