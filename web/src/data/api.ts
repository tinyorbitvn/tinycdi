import { unwrap, type ApiClient } from "../api/client";
import { isPortalApiError } from "../api/errors";
import type { components } from "../api/generated/schema";
import { get, type Scope } from "../app/me";
import type { ServerPage } from "../design/useServerPage";
import { listAllTemplates } from "../templates/useTemplates";

// Data-area API surface, called through the same openapi-fetch client (CSRF
// middleware, same-origin credentials). The principal reads and the
// loader plumbing (Me/useLoader) are shared and live in ../app/me.

export type WorkspaceView = components["schemas"]["WorkspaceView"];
export type TemplateView = components["schemas"]["TemplateView"];
export type RetainedDataView = components["schemas"]["RetainedDataView"];
export type RetainedDataState = components["schemas"]["RetainedDataState"];
type RuntimeKind = components["schemas"]["RuntimeKind"];

export type { Scope };
export type ScopedRetainedData = RetainedDataView;

export interface AttachDataBody {
  name: string;
  templateRef: string;
  desiredState?: "Running" | "Stopped";
}

/** Server default order of GET /v1/data: most recently retained first. */
export const DATA_DEFAULT_SORT = "-retainedAt";

// One page of GET /v1/data under `sort` (`retainedAt` | `name`, the source
// workspace name; `-` = descending). `Purged` records are excluded
// server-side.
export async function listRetainedDataPage(
  api: ApiClient,
  scope: Scope,
  cursor?: string,
  sort: string = DATA_DEFAULT_SORT,
  limit?: number,
): Promise<ServerPage<ScopedRetainedData>> {
  return unwrap(
    await api.GET("/v1/data", {
      params: {
        query: { scope, sort, ...(limit ? { limit } : {}), ...(cursor ? { cursor } : {}) },
      },
    }),
  );
}

// GET /v1/data/{dataId}: one record, visible to its owner and to a tenant
// admin of the same tenant. Any other caller gets 404 — existence is never
// leaked — which reads as "not found" (null) here.
export async function getRetainedData(
  api: ApiClient,
  id: string,
): Promise<ScopedRetainedData | null> {
  try {
    return await get<ScopedRetainedData>(api, `/v1/data/${encodeURIComponent(id)}`);
  } catch (e) {
    if (isPortalApiError(e) && e.httpStatus === 404) return null;
    throw e;
  }
}

export async function attachRetainedData(
  api: ApiClient,
  dataId: string,
  body: AttachDataBody,
  idempotencyKey: string,
): Promise<WorkspaceView> {
  return unwrap(
    await api.POST("/v1/data/{dataId}:attach", {
      params: {
        path: { dataId },
        header: { "Idempotency-Key": idempotencyKey },
      },
      body,
    }),
  );
}

export async function purgeRetainedData(
  api: ApiClient,
  dataId: string,
  confirmationNonce: string,
  idempotencyKey?: string,
): Promise<RetainedDataView> {
  return unwrap(
    await api.POST("/v1/data/{dataId}:purge", {
      params: {
        path: { dataId },
        ...(idempotencyKey
          ? { header: { "Idempotency-Key": idempotencyKey } }
          : {}),
      },
      body: { confirmationNonce },
    }),
  );
}

// Templates eligible for attach are constrained to the disk's runtime
// (attach returns 422 INVALID_TEMPLATE otherwise).
export async function listTemplates(api: ApiClient, runtime?: RuntimeKind): Promise<TemplateView[]> {
  const items = await listAllTemplates(api, runtime ? { runtime } : {});
  return items.filter((tpl) => tpl.publishedAt);
}
