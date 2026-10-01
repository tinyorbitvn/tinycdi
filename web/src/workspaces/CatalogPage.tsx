import { useEffect, useState } from "react";
import { useApi } from "../api/context";
import { unwrap } from "../api/client";
import { isPortalApiError } from "../api/errors";
import { Link } from "../lib/router";
import type { TemplateView } from "./helpers";
import { ErrorBanner } from "./ErrorBanner";

export function CatalogPage() {
  const api = useApi();
  const [templates, setTemplates] = useState<TemplateView[] | null>(null);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = unwrap(await api.GET("/v1/templates", {}));
        if (!cancelled) setTemplates(res.items);
      } catch (e) {
        if (!cancelled) setError(e);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api]);

  if (error) {
    if (isPortalApiError(error) && error.code === "UNAUTHENTICATED") {
      window.location.assign("/v1/login");
      return null;
    }
    return (
      <main>
        <ErrorBanner error={error} onDismiss={() => setError(null)} />
      </main>
    );
  }
  if (!templates) return <main aria-busy="true">Loading catalog…</main>;

  return (
    <main>
      <h1>Template catalog</h1>
      {templates.length === 0 ? <p>No templates published.</p> : null}
      <ul className="catalog">
        {templates.map((t) => (
          <li key={`${t.id}@${t.revision}`}>
            <strong>{t.name}</strong> <small>({t.id} rev {t.revision})</small>
            {t.description ? <p>{t.description}</p> : null}
            <dl>
              <dt>Runtime</dt>
              <dd>{t.runtime}</dd>
              <dt>Experience</dt>
              <dd>{t.experience}</dd>
              <dt>Resources</dt>
              <dd>
                {t.resources.cpuMillicores}m CPU · {t.resources.memoryMib} MiB ·{" "}
                {t.resources.storageGib} GiB
              </dd>
              <dt>Data policy default</dt>
              <dd>{t.dataPolicyDefault}</dd>
              <dt>Clipboard</dt>
              <dd>{t.clipboardPolicy}</dd>
            </dl>
            <Link to={`/workspaces/new?template=${t.id}`}>Create workspace</Link>
          </li>
        ))}
      </ul>
    </main>
  );
}
