import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Dialog, Input, Stack } from "../design";
import { useApi } from "../api/context";
import { isPortalApiError } from "../api/errors";
import { t } from "../i18n";
import {
  getRetainedData,
  purgeRetainedData,
  type ScopedRetainedData,
} from "./api";

// Purge is irreversible and contractually two-step: the dialog re-reads the
// record to obtain a fresh purgeConfirmationNonce (every read mints a new
// one, single-use and bound to the caller), and requires typing the data ID
// before the confirm button activates. A 409 INVALID_STATE means a
// concurrent attach won the race — the disk is no longer purgable.
export function PurgeDialog({
  record,
  onClose,
  onPurged,
}: {
  record: ScopedRetainedData;
  onClose: () => void;
  onPurged: () => void;
}) {
  const api = useApi();
  const [typed, setTyped] = useState("");
  const [nonce, setNonce] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const refreshNonce = useCallback(async () => {
    const fresh = await getRetainedData(api, record.id);
    if (!fresh) throw new Error("gone");
    setNonce(fresh.purgeConfirmationNonce);
  }, [api, record.id]);

  useEffect(() => {
    let cancelled = false;
    getRetainedData(api, record.id).then(
      (fresh) => {
        if (cancelled) return;
        if (fresh) setNonce(fresh.purgeConfirmationNonce);
        else setError(new Error("gone"));
      },
      (e: unknown) => {
        if (!cancelled) setError(e);
      },
    );
    return () => {
      cancelled = true;
    };
  }, [api, record.id]);

  const confirmed = typed === record.id && nonce !== null;

  async function purge() {
    if (!confirmed || !nonce) return;
    setBusy(true);
    setError(null);
    try {
      await purgeRetainedData(api, record.id, nonce);
      onPurged();
    } catch (e) {
      setError(e);
      // The nonce is consumed even on failure — silently re-read so a
      // retry (or a corrected attempt) uses a fresh one.
      setNonce(null);
      refreshNonce().catch(() => {});
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog
      open
      onClose={onClose}
      dismissible={!busy}
      role="alertdialog"
      title={t("data.purge.title", { id: record.id })}
      description={t("data.purge.description", {
        id: record.id,
        size: record.sizeGib,
        source: record.sourceWorkspaceName,
      })}
      footer={
        <>
          <Button onClick={onClose} disabled={busy}>
            {t("data.action.cancel")}
          </Button>
          <Button
            variant="danger"
            loading={busy}
            disabled={!confirmed}
            onClick={() => void purge()}
          >
            {t("data.purge.submit")}
          </Button>
        </>
      }
    >
      <Stack gap={4}>
        <Input
          label={t("data.purge.confirmLabel")}
          hint={t("data.purge.confirmHint", { id: record.id })}
          required
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          autoComplete="off"
        />
        {error ? (
          <Alert
            tone="danger"
            title={isPortalApiError(error) ? error.code : undefined}
          >
            {isPortalApiError(error) && error.code === "INVALID_STATE"
              ? t("data.purge.conflict")
              : isPortalApiError(error)
                ? error.message
                : t("data.purge.notFound")}
          </Alert>
        ) : null}
      </Stack>
    </Dialog>
  );
}
