import { useCallback, useEffect, useState } from "react";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { unwrap } from "../api/client";
import { Link } from "../lib/router";
import type { RetainedDataView } from "./helpers";
import { ErrorBanner } from "./ErrorBanner";

// Purge is irreversible and contractually two-step: the dialog re-reads the
// record to obtain a fresh purgeConfirmationNonce and requires typing the
// source workspace name before the confirm button activates.
export function PurgeDialog({
  record,
  onClose,
  onPurged,
}: {
  record: RetainedDataView;
  onClose: () => void;
  onPurged: () => void;
}) {
  const api = useApi();
  const [typed, setTyped] = useState("");
  const [nonce, setNonce] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = unwrap(await api.GET("/v1/data", {}));
        const fresh = res.items.find((r) => r.id === record.id);
        if (!cancelled) {
          if (fresh) setNonce(fresh.purgeConfirmationNonce);
          else setError(new Error("record no longer visible"));
        }
      } catch (e) {
        if (!cancelled) setError(e);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, record.id]);

  const confirmed = typed === record.sourceWorkspaceName && nonce !== null;

  async function purge() {
    if (!confirmed || !nonce) return;
    setBusy(true);
    setError(null);
    try {
      unwrap(
        await api.POST("/v1/data/{dataId}/purge", {
          params: { path: { dataId: record.id } },
          body: { confirmationNonce: nonce },
        }),
      );
      onPurged();
    } catch (e) {
      setError(e);
      setNonce(null);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div
      role="dialog"
      aria-label={t("data.purge.label")}
      className="dialog"
    >
      <p>
        {t("data.purge.warning", {
          id: record.id,
          size: record.sizeGib,
          name: record.sourceWorkspaceName,
        })}
      </p>
      <label>
        {t("data.purge.confirmLabel")}
        <input
          name="purge-confirm"
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          autoComplete="off"
        />
      </label>
      <ErrorBanner error={error} onDismiss={() => setError(null)} />
      <button
        type="button"
        className="danger"
        disabled={!confirmed || busy}
        onClick={() => void purge()}
      >
        {busy ? t("data.purge.confirming") : t("data.purge.confirm")}
      </button>
      <button type="button" disabled={busy} onClick={onClose}>
        {t("common.cancel")}
      </button>
    </div>
  );
}

export function DataPage() {
  const api = useApi();
  const [items, setItems] = useState<RetainedDataView[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [purging, setPurging] = useState<RetainedDataView | null>(null);

  const refresh = useCallback(async () => {
    try {
      const res = unwrap(await api.GET("/v1/data", {}));
      setItems(res.items);
      setError(null);
    } catch (e) {
      setError(e);
    }
  }, [api]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  return (
    <main>
      <h1>{t("data.list.title")}</h1>
      <p>{t("data.list.intro")}</p>
      <ErrorBanner error={error} onDismiss={() => setError(null)} />
      {!items ? (
        <p aria-busy="true">{t("data.list.loading")}</p>
      ) : items.length === 0 ? (
        <p>{t("data.list.empty")}</p>
      ) : (
        <table aria-label={t("data.list.label")}>
          <thead>
            <tr>
              <th>{t("data.list.col.id")}</th>
              <th>{t("data.list.col.workspace")}</th>
              <th>{t("data.list.col.runtime")}</th>
              <th>{t("data.list.col.size")}</th>
              <th>{t("data.list.col.state")}</th>
              <th>{t("data.list.col.retainedAt")}</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {items.map((r) => (
              <tr key={r.id}>
                <td>{r.id}</td>
                <td>{r.sourceWorkspaceName}</td>
                <td>{r.runtime}</td>
                <td>{t("data.list.size", { size: r.sizeGib })}</td>
                <td>{r.state}</td>
                <td>{new Date(r.retainedAt).toLocaleString()}</td>
                <td>
                  {r.state === "Retained" ? (
                    <button
                      type="button"
                      className="danger"
                      onClick={() => setPurging(r)}
                    >
                      {t("data.purge.action")}
                    </button>
                  ) : null}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {purging ? (
        <PurgeDialog
          record={purging}
          onClose={() => setPurging(null)}
          onPurged={() => {
            setPurging(null);
            void refresh();
          }}
        />
      ) : null}
      <p>
        <Link to="/">{t("nav.allWorkspaces")}</Link>
      </p>
    </main>
  );
}
