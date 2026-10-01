// Shared ports for the portal-CSP harness (tests-portal): the real
// portal binary on https://localhost:4174, the contract mock API on
// http://127.0.0.1:4320, and the mock session origin on
// https://127.0.0.1:4312. Portal on `localhost` and session on `127.0.0.1`
// are different SITES — the production shape — so the launch form POST is
// genuinely cross-site and SameSite semantics apply exactly as they do in
// production. Separate ports from playwright.config (4173/4310/4311) so
// both suites can coexist.

export const PORTAL_PORT = Number(process.env.PORTAL_E2E_PORT ?? 4174);
export const FRONTEND_PORT = Number(process.env.PORTAL_E2E_FRONTEND_PORT ?? 4175);
export const API_PORT = Number(process.env.PORTAL_E2E_API_PORT ?? 4320);
export const SESSION_PORT = Number(process.env.PORTAL_E2E_SESSION_PORT ?? 4312);
export const PORTAL_ORIGIN = `https://localhost:${PORTAL_PORT}`;
export const SESSION_ORIGIN = `https://127.0.0.1:${SESSION_PORT}`;
export const MOCK_API = `http://127.0.0.1:${API_PORT}`;
