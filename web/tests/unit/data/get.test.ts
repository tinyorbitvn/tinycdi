import { describe, expect, it } from "vitest";
import { createApi } from "../../../src/api/client";
import { getRetainedData } from "../../../src/data/api";
import { createMockApi, loginCookies, stubFetch } from "../helpers";
import { makeAdmin, seedTenant } from "../admin/helpers";
import { RETAINED_DISK } from "../../mock-api/fixtures.ts";

function dataGets(api: ReturnType<typeof createMockApi>): string[] {
  return api.state.requests.filter((r) => r.method === "GET" && r.path.startsWith("/v1/data")).map((r) => r.path);
}

describe("getRetainedData", () => {
  it("reads one record with GET /v1/data/{id}, without walking the list", async () => {
    const api = createMockApi();
    loginCookies();
    const rec = await getRetainedData(createApi(stubFetch(api)), RETAINED_DISK.id);

    expect(rec?.id).toBe(RETAINED_DISK.id);
    expect(rec?.purgeConfirmationNonce).toBeTruthy();
    expect(dataGets(api)).toEqual([`/v1/data/${RETAINED_DISK.id}`]);
  });

  it("maps 404 to null (unknown id, or a record the caller may not see)", async () => {
    const api = createMockApi();
    loginCookies();
    expect(await getRetainedData(createApi(stubFetch(api)), "rd_doesnotexist0001")).toBeNull();
    expect(dataGets(api)).toEqual(["/v1/data/rd_doesnotexist0001"]);
  });

  it("lets a tenant admin read another user's record in one request", async () => {
    const api = createMockApi();
    loginCookies();
    seedTenant(api);
    const foreign = [...api.state.retained.values()].find((r) => r.id !== RETAINED_DISK.id)!;
    makeAdmin(api);
    api.state.requests = [];
    const rec = await getRetainedData(createApi(stubFetch(api)), foreign.id);

    expect(rec?.id).toBe(foreign.id);
    expect(dataGets(api)).toEqual([`/v1/data/${foreign.id}`]);
  });

  it("reads another user's record as null for a regular user", async () => {
    const api = createMockApi();
    loginCookies();
    seedTenant(api);
    const foreign = [...api.state.retained.values()].find((r) => r.id !== RETAINED_DISK.id)!;
    expect(await getRetainedData(createApi(stubFetch(api)), foreign.id)).toBeNull();
  });
});
