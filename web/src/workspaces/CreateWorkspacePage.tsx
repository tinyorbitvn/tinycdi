import { useEffect, useRef, useState, type FormEvent } from "react";
import { useApi } from "../api/context";
import { newIdempotencyKey, unwrap } from "../api/client";
import { isPortalApiError } from "../api/errors";
import { navigate, Link } from "../lib/router";
import type { TemplateView } from "./helpers";
import { ErrorBanner } from "./ErrorBanner";

type DataPolicy = "Ephemeral" | "Retain";

export function CreateWorkspacePage() {
  const api = useApi();
  const [templates, setTemplates] = useState<TemplateView[] | null>(null);
  const [name, setName] = useState("");
  const [templateRef, setTemplateRef] = useState("");
  const [dataPolicy, setDataPolicy] = useState<DataPolicy | "">("");
  const [startNow, setStartNow] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [loadError, setLoadError] = useState<unknown>(null);
  // One Idempotency-Key per create attempt; a retry of the SAME attempt reuses
  // it so the server replays the recorded result instead of double-creating.
  const idemKey = useRef<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = unwrap(await api.GET("/v1/templates", {}));
        if (!cancelled) {
          setTemplates(res.items);
          const preset = new URLSearchParams(window.location.search).get("template");
          if (preset && res.items.some((t) => t.id === preset)) setTemplateRef(preset);
        }
      } catch (e) {
        if (!cancelled) setLoadError(e);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api]);

  const selected = templates?.find((t) => t.id === templateRef);

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!templateRef || !name.trim()) return;
    setBusy(true);
    setError(null);
    idemKey.current ??= newIdempotencyKey();
    try {
      const ws = unwrap(
        await api.POST("/v1/workspaces", {
          params: { header: { "Idempotency-Key": idemKey.current } },
          body: {
            name: name.trim(),
            templateRef,
            desiredState: startNow ? "Running" : "Stopped",
            ...(dataPolicy ? { dataPolicy: dataPolicy as DataPolicy } : {}),
          },
        }),
      );
      idemKey.current = null;
      navigate(`/workspaces/${ws.id}`);
    } catch (err) {
      setError(err);
      // Key reused with a different body: this attempt is over — the next
      // submit mints a fresh key.
      if (isPortalApiError(err) && err.code === "IDEMPOTENCY_CONFLICT") {
        idemKey.current = null;
      }
    } finally {
      setBusy(false);
    }
  }

  if (loadError) {
    return (
      <main>
        <ErrorBanner error={loadError} onDismiss={() => setLoadError(null)} />
      </main>
    );
  }

  return (
    <main>
      <h1>New workspace</h1>
      <ErrorBanner error={error} onDismiss={() => setError(null)} />
      {!templates ? (
        <p aria-busy="true">Loading templates…</p>
      ) : (
        <form onSubmit={(e) => void submit(e)}>
          <label>
            Name
            <input
              name="name"
              required
              pattern="[a-z0-9][a-z0-9\-]{0,126}[a-z0-9]|[a-z0-9]"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="research-desktop"
            />
          </label>
          <label>
            Template
            <select
              name="template"
              required
              value={templateRef}
              onChange={(e) => setTemplateRef(e.target.value)}
            >
              <option value="" disabled>
                Select a template
              </option>
              {templates.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name} (rev {t.revision}, {t.runtime})
                </option>
              ))}
            </select>
          </label>
          <label>
            Data policy
            <select
              name="dataPolicy"
              value={dataPolicy}
              onChange={(e) => setDataPolicy(e.target.value as DataPolicy)}
            >
              <option value="">
                Template default{selected ? ` (${selected.dataPolicyDefault})` : ""}
              </option>
              <option value="Retain">Retain — keep disk on stop/delete</option>
              <option value="Ephemeral">Ephemeral — destroy data on stop/delete</option>
            </select>
          </label>
          <label>
            <input
              type="checkbox"
              checked={startNow}
              onChange={(e) => setStartNow(e.target.checked)}
            />
            Start immediately
          </label>
          <button type="submit" disabled={busy || !name.trim() || !templateRef}>
            {busy ? "Creating…" : "Create workspace"}
          </button>
        </form>
      )}
      <p>
        <Link to="/">← All workspaces</Link>
      </p>
    </main>
  );
}
