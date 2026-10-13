import { describe, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { useServerPage, type ServerPage } from "../../../src/design/useServerPage";

type Row = { id: string };

// A two-page server: the first page hands out cursor "c1", the second page
// is the last one (no nextCursor). Every call is recorded with its sort.
function twoPageServer() {
  const calls: Array<{ cursor: string | undefined; sort: string | undefined }> = [];
  const fetchPage = vi.fn(async (cursor?: string, sort?: string): Promise<ServerPage<Row>> => {
    calls.push({ cursor, sort });
    const tag = sort ?? "";
    if (cursor === undefined) return { items: [{ id: `${tag}:a` }, { id: `${tag}:b` }], nextCursor: "c1" };
    if (cursor === "c1") return { items: [{ id: `${tag}:c` }] };
    throw new Error(`unexpected cursor ${cursor}`);
  });
  return { calls, fetchPage };
}

describe("design/useServerPage", () => {
  it("next() appends the second page using nextCursor", async () => {
    const { calls, fetchPage } = twoPageServer();
    const { result } = renderHook(() => useServerPage(fetchPage, { sort: "name" }));

    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.items.map((r) => r.id)).toEqual(["name:a", "name:b"]);
    expect(result.current.hasNext).toBe(true);

    act(() => result.current.next());
    await waitFor(() => expect(result.current.items).toHaveLength(3));
    expect(result.current.items.map((r) => r.id)).toEqual(["name:a", "name:b", "name:c"]);
    expect(calls).toEqual([
      { cursor: undefined, sort: "name" },
      { cursor: "c1", sort: "name" },
    ]);
  });

  it("no nextCursor gives hasNext=false and next() is a no-op", async () => {
    const fetchPage = vi.fn(async (): Promise<ServerPage<Row>> => ({ items: [{ id: "only" }] }));
    const { result } = renderHook(() => useServerPage(fetchPage, { sort: "name" }));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.hasNext).toBe(false);
    act(() => result.current.next());
    expect(fetchPage).toHaveBeenCalledTimes(1);
  });

  it('reset("-name") clears items and refetches with the new sort', async () => {
    const { calls, fetchPage } = twoPageServer();
    const { result } = renderHook(() => useServerPage(fetchPage, { sort: "name" }));
    await waitFor(() => expect(result.current.loading).toBe(false));
    act(() => result.current.next());
    await waitFor(() => expect(result.current.items).toHaveLength(3));

    act(() => result.current.reset("-name"));
    expect(result.current.items).toEqual([]);
    expect(result.current.loading).toBe(true);
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.items.map((r) => r.id)).toEqual(["-name:a", "-name:b"]);
    expect(result.current.sort).toBe("-name");
    expect(calls.at(-1)).toEqual({ cursor: undefined, sort: "-name" });
  });

  it("drops a page that arrives after a reset", async () => {
    let release: (p: ServerPage<Row>) => void = () => {};
    const fetchPage = vi.fn((cursor?: string, sort?: string): Promise<ServerPage<Row>> => {
      if (sort === "name") return new Promise((r) => (release = r));
      return Promise.resolve({ items: [{ id: `fresh:${cursor ?? ""}` }] });
    });
    const { result } = renderHook(() => useServerPage(fetchPage, { sort: "name" }));
    act(() => result.current.reset("-name"));
    await waitFor(() => expect(result.current.items.map((r) => r.id)).toEqual(["fresh:"]));
    await act(async () => release({ items: [{ id: "stale" }] }));
    expect(result.current.items.map((r) => r.id)).toEqual(["fresh:"]);
  });

  it("surfaces a fetch error and keeps loaded rows", async () => {
    const fetchPage = vi.fn(async (cursor?: string): Promise<ServerPage<Row>> => {
      if (cursor) throw new Error("boom");
      return { items: [{ id: "a" }], nextCursor: "c1" };
    });
    const { result } = renderHook(() => useServerPage(fetchPage, { sort: "name" }));
    await waitFor(() => expect(result.current.loading).toBe(false));
    act(() => result.current.next());
    await waitFor(() => expect(result.current.error).toBeInstanceOf(Error));
    expect(result.current.items.map((r) => r.id)).toEqual(["a"]);
    expect(result.current.hasNext).toBe(true);
  });

  it("refresh() re-reads as many pages as are loaded, without clearing", async () => {
    const { calls, fetchPage } = twoPageServer();
    const { result } = renderHook(() => useServerPage(fetchPage, { sort: "name" }));
    await waitFor(() => expect(result.current.loading).toBe(false));
    act(() => result.current.next());
    await waitFor(() => expect(result.current.items).toHaveLength(3));
    calls.length = 0;

    act(() => result.current.refresh());
    expect(result.current.items).toHaveLength(3);
    await waitFor(() => expect(calls).toHaveLength(2));
    expect(calls).toEqual([
      { cursor: undefined, sort: "name" },
      { cursor: "c1", sort: "name" },
    ]);
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.items).toHaveLength(3);
  });

  it("a changed key restarts from the first page", async () => {
    const { calls, fetchPage } = twoPageServer();
    const { result, rerender } = renderHook(({ k }) => useServerPage(fetchPage, { sort: "name", key: k }), {
      initialProps: { k: "a" },
    });
    await waitFor(() => expect(result.current.loading).toBe(false));
    rerender({ k: "b" });
    await waitFor(() => expect(calls).toHaveLength(2));
    expect(calls[1]).toEqual({ cursor: undefined, sort: "name" });
  });

  it("poll refreshes while it returns a delay and stops on null", async () => {
    let calls = 0;
    const fetchPage = vi.fn(async (): Promise<ServerPage<Row>> => {
      calls += 1;
      return { items: [{ id: calls < 3 ? "busy" : "idle" }] };
    });
    const { result } = renderHook(() =>
      useServerPage(fetchPage, { sort: "name", poll: (items) => (items.some((r) => r.id === "busy") ? 5 : null) }),
    );
    await waitFor(() => expect(result.current.items.map((r) => r.id)).toEqual(["idle"]));
    await new Promise((r) => setTimeout(r, 40));
    expect(fetchPage).toHaveBeenCalledTimes(3);
  });

  it("a failed first page keeps polling and recovers", async () => {
    let calls = 0;
    const fetchPage = vi.fn(async (): Promise<ServerPage<Row>> => {
      calls += 1;
      if (calls === 1) throw new Error("down");
      return { items: [{ id: "back" }] };
    });
    const { result } = renderHook(() => useServerPage(fetchPage, { sort: "name", poll: () => 5 }));
    await waitFor(() => expect(result.current.items.map((r) => r.id)).toEqual(["back"]));
    expect(result.current.error).toBeNull();
    expect(result.current.loaded).toBe(true);
  });
});
