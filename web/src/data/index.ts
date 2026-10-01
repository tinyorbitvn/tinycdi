// Data area: retained-disk list/detail, attach and purge flows.
// `routes` comes from ./routes.tsx (seeded by the app shell); the pages and
// the API surface live in this module.
export { routes } from "./routes";
export { DataListPage, DataStateBadge, canAttach, canPurge, ownerLabel } from "./DataListPage";
export { DataDetailPage } from "./DataDetailPage";
export { AttachDialog } from "./AttachDialog";
export { PurgeDialog } from "./PurgeDialog";
export * from "./api";
