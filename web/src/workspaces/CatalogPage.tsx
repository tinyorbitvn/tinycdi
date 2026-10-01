import { useEffect, useState } from "react";
import { t } from "../i18n";
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
  if (!templates) {
    return <main aria-busy="true">{t("templates.catalog.loading")}</main>;
  }

  return (
    <main>
      <h1>{t("templates.catalog.title")}</h1>
      {templates.length === 0 ? <p>{t("templates.catalog.empty")}</p> : null}
      <ul className="catalog">
        {templates.map((tpl) => (
          <li key={`${tpl.id}@${tpl.revision}`}>
            <strong>{tpl.name}</strong>{" "}
            <small>
              {t("templates.catalog.revision", {
                id: tpl.id,
                revision: tpl.revision,
              })}
            </small>
            {tpl.description ? <p>{tpl.description}</p> : null}
            <dl>
              <dt>{t("templates.catalog.field.runtime")}</dt>
              <dd>{tpl.runtime}</dd>
              <dt>{t("templates.catalog.field.experience")}</dt>
              <dd>{tpl.experience}</dd>
              <dt>{t("templates.catalog.field.resources")}</dt>
              <dd>
                {t("templates.catalog.resources", {
                  cpu: tpl.resources.cpuMillicores,
                  memory: tpl.resources.memoryMib,
                  storage: tpl.resources.storageGib,
                })}
              </dd>
              <dt>{t("templates.catalog.field.dataPolicy")}</dt>
              <dd>{tpl.dataPolicyDefault}</dd>
              <dt>{t("templates.catalog.field.clipboard")}</dt>
              <dd>{tpl.clipboardPolicy}</dd>
            </dl>
            <Link to={`/workspaces/new?template=${tpl.id}`}>
              {t("templates.catalog.create")}
            </Link>
          </li>
        ))}
      </ul>
    </main>
  );
}
