import { useEffect, useRef, useState, type FormEvent } from "react";
import { t } from "../i18n";
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
          if (preset && res.items.some((tpl) => tpl.id === preset)) setTemplateRef(preset);
        }
      } catch (e) {
        if (!cancelled) setLoadError(e);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api]);

  const selected = templates?.find((tpl) => tpl.id === templateRef);

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
      <h1>{t("workspaces.create.title")}</h1>
      <ErrorBanner error={error} onDismiss={() => setError(null)} />
      {!templates ? (
        <p aria-busy="true">{t("workspaces.create.loading")}</p>
      ) : (
        <form onSubmit={(e) => void submit(e)}>
          <label>
            {t("workspaces.create.nameLabel")}
            <input
              name="name"
              required
              pattern="[a-z0-9][a-z0-9\-]{0,126}[a-z0-9]|[a-z0-9]"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={t("workspaces.create.namePlaceholder")}
            />
          </label>
          <label>
            {t("workspaces.create.templateLabel")}
            <select
              name="template"
              required
              value={templateRef}
              onChange={(e) => setTemplateRef(e.target.value)}
            >
              <option value="" disabled>
                {t("workspaces.create.templatePlaceholder")}
              </option>
              {templates.map((tpl) => (
                <option key={tpl.id} value={tpl.id}>
                  {t("workspaces.create.templateOption", {
                    name: tpl.name,
                    revision: tpl.revision,
                    runtime: tpl.runtime,
                  })}
                </option>
              ))}
            </select>
          </label>
          <label>
            {t("workspaces.create.dataPolicyLabel")}
            <select
              name="dataPolicy"
              value={dataPolicy}
              onChange={(e) => setDataPolicy(e.target.value as DataPolicy)}
            >
              <option value="">
                {selected
                  ? t("workspaces.create.dataPolicyDefaultNamed", {
                      policy: selected.dataPolicyDefault,
                    })
                  : t("workspaces.create.dataPolicyDefault")}
              </option>
              <option value="Retain">
                {t("workspaces.create.dataPolicyRetain")}
              </option>
              <option value="Ephemeral">
                {t("workspaces.create.dataPolicyEphemeral")}
              </option>
            </select>
          </label>
          <label>
            <input
              type="checkbox"
              checked={startNow}
              onChange={(e) => setStartNow(e.target.checked)}
            />
            {t("workspaces.create.startNow")}
          </label>
          <button type="submit" disabled={busy || !name.trim() || !templateRef}>
            {busy
              ? t("workspaces.create.submitting")
              : t("workspaces.create.submit")}
          </button>
        </form>
      )}
      <p>
        <Link to="/">{t("nav.allWorkspaces")}</Link>
      </p>
    </main>
  );
}
