import { useEffect, useRef, useState } from "react";
import { Alert, Button, Dialog, Input, Select, Stack } from "../design";
import { useApi } from "../api/context";
import { newIdempotencyKey } from "../api/client";
import { isPortalApiError, isReleasePending } from "../api/errors";
import { t } from "../i18n";
import { useLoader } from "../app/me";
import {
  attachRetainedData,
  listTemplates,
  type ScopedRetainedData,
  type WorkspaceView,
} from "./api";

// Attach consumes the disk exclusively: the dialog creates a new workspace
// whose disk is the retained record (the record moves to Attaching and a
// concurrent purge loses the race). One Idempotency-Key per dialog session —
// a retried submit of the SAME attempt reuses it.
export function AttachDialog({
  record,
  onClose,
  onAttached,
}: {
  record: ScopedRetainedData;
  onClose: () => void;
  onAttached: (ws: WorkspaceView) => void;
}) {
  const api = useApi();
  const templates = useLoader(
    (a) => listTemplates(a, record.runtime),
    `tpl:${record.runtime}`,
  );
  const [name, setName] = useState(`${record.sourceWorkspaceName}-restored`);
  const [templateRef, setTemplateRef] = useState("");
  const [desiredState, setDesiredState] = useState<"Running" | "Stopped">("Running");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const idemKey = useRef<string | undefined>(undefined);

  const eligible = templates.data?.items ?? [];
  const selected = eligible.find((tpl) => tpl.id === templateRef) ?? eligible[0];

  useEffect(() => {
    if (selected && templateRef !== selected.id) setTemplateRef(selected.id);
  }, [selected, templateRef]);

  // Mirrors workspaceNamePattern in internal/api/workspaces.go.
  const nameOk = /^[a-z0-9]$|^[a-z0-9][a-z0-9-]{0,125}[a-z0-9]$/.test(name);
  const canSubmit = !!selected && nameOk && !busy;

  async function submit() {
    if (!canSubmit || !selected) return;
    setBusy(true);
    setError(null);
    try {
      idemKey.current ??= newIdempotencyKey();
      const ws = await attachRetainedData(
        api,
        record.id,
        { name, templateRef: selected.id, desiredState },
        idemKey.current,
      );
      onAttached(ws);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog
      open
      onClose={onClose}
      dismissible={!busy}
      title={t("data.attach.title")}
      description={t("data.attach.description", {
        id: record.id,
        size: record.sizeGib,
      })}
      footer={
        <>
          <Button onClick={onClose} disabled={busy}>
            {t("data.action.cancel")}
          </Button>
          <Button
            variant="primary"
            loading={busy}
            disabled={!canSubmit}
            onClick={() => void submit()}
          >
            {t("data.attach.submit")}
          </Button>
        </>
      }
    >
      <Stack gap={4}>
        <Input
          label={t("data.attach.name.label")}
          hint={t("data.attach.name.hint")}
          required
          value={name}
          onChange={(e) => setName(e.target.value)}
          error={name && !nameOk ? t("data.attach.name.invalid") : undefined}
        />
        {templates.data && eligible.length === 0 ? (
          <Alert tone="warning">{t("data.attach.noTemplates", { runtime: record.runtime })}</Alert>
        ) : (
          <Select
            label={t("data.attach.template.label")}
            hint={t("data.attach.template.hint", { runtime: record.runtime })}
            required
            value={selected?.id ?? ""}
            onChange={(e) => setTemplateRef(e.target.value)}
            options={eligible.map((tpl) => ({ value: tpl.id, label: tpl.name }))}
          />
        )}
        <Select
          label={t("data.attach.desiredState.label")}
          value={desiredState}
          onChange={(e) => setDesiredState(e.target.value as "Running" | "Stopped")}
          options={[
            { value: "Running", label: t("data.attach.desiredState.running") },
            { value: "Stopped", label: t("data.attach.desiredState.stopped") },
          ]}
        />
        {templates.error ? (
          <Alert tone="danger">
            {templates.error instanceof Error ? templates.error.message : String(templates.error)}
          </Alert>
        ) : null}
        {error ? (
          <Alert tone="danger" title={isPortalApiError(error) ? error.code : undefined}>
            {isReleasePending(error)
              ? t("errors.code.quotaReleasePending")
              : isPortalApiError(error) && error.code === "INVALID_STATE"
                ? t("data.attach.conflict")
                : error instanceof Error
                  ? error.message
                  : String(error)}
          </Alert>
        ) : null}
      </Stack>
    </Dialog>
  );
}
