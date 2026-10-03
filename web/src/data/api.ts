import { unwrap, type ApiClient } from "../api/client";
import { isPortalApiError } from "../api/errors";
import type { components } from "../api/generated/schema";
import { get, listAll, type Scope } from "../app/me";

// Data-area API surface, called through the same openapi-fetch client (CSRF
// middleware, same-origin credentials). The principal reads and the
// loader/list plumbing (Me/useLoader/listAll) are shared and live in
// ../app/me.

export type WorkspaceView = components["schemas"]["WorkspaceView"];
export type TemplateView = components["schemas"]["TemplateView"];
export type RetainedDataView = components["schemas"]["RetainedDataView"];
export type RetainedDataState = components["schemas"]["RetainedDataState"];

export type { Scope };
export type ScopedRetainedData = RetainedDataView;

export interface AttachDataBody {
  name: string;
  templateRef: string;
  desiredState?: "Running" | "Stopped";
}

// GET /v1/data. `Purged` records are excluded server-side; the filter here
// keeps the list honest if an old backend still returns them.
export async function listRetainedData(
  api: ApiClient,
  scope: Scope,
): Promise<{ items: ScopedRetainedData[]; truncated: boolean }> {
  const page = await listAll<ScopedRetainedData>(api, "/v1/data", { scope });
  return { ...page, items: page.items.filter((r) => r.state !== "Purged") };
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
    await api.POST("/v1/data/{dataId}/attach", {
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
    await api.POST("/v1/data/{dataId}/purge", {
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
export function listTemplates(api: ApiClient, runtime?: string) {
  return listAll<TemplateView>(api, "/v1/templates", { runtime }).then((p) => ({
    ...p,
    items: p.items.filter(
      (tpl) => tpl.publishedAt && (runtime === undefined || tpl.runtime === runtime),
    ),
  }));
}
