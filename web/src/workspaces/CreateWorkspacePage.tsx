import { useRef, useState, type FormEvent } from "react";
import { Alert, Button, Checkbox, Input, Page, Select, Spinner } from "../design";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { newIdempotencyKey, unwrap } from "../api/client";
import { isPortalApiError } from "../api/errors";
import { navigate, Link } from "../lib/router";
import { useTemplates } from "../templates/useTemplates";
import { dataPolicyLabel, networkProfileLabel } from "../templates/format";
import type { DataPolicy } from "../templates/types";
import { ErrorBanner } from "./ErrorBanner";

// DNS-label display name (same client-side shape the API enforces).
const NAME_PATTERN = "[a-z0-9][a-z0-9\\-]{0,126}[a-z0-9]|[a-z0-9]";

type PolicyChoice = "" | DataPolicy;

export function CreateWorkspacePage() {
  const api = useApi();
  const templates = useTemplates();
  const [name, setName] = useState("");
  const [templateRef, setTemplateRef] = useState(() => {
    // ?template=<id> preselects from the catalog ("Create workspace" cards).
    return new URLSearchParams(window.location.search).get("template") ?? "";
  });
  const [dataPolicy, setDataPolicy] = useState<PolicyChoice>("");
  const [startNow, setStartNow] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  // One Idempotency-Key per create attempt; a retry of the SAME attempt reuses
  // it so the server replays the recorded result instead of double-creating.
  const idemKey = useRef<string | null>(null);

  const selected = templates.data?.find((tpl) => tpl.id === templateRef);
  const effectivePolicy: DataPolicy | undefined = dataPolicy || selected?.dataPolicyDefault;

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
            ...(dataPolicy ? { dataPolicy } : {}),
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

  const policyOptions = [
    {
      value: "",
      label: selected
        ? t("workspaces.create.dataPolicyDefaultNamed", {
            policy: dataPolicyLabel(selected.dataPolicyDefault),
          })
        : t("workspaces.create.dataPolicyDefault"),
    },
    { value: "Retain", label: t("workspaces.create.dataPolicyRetain") },
    { value: "Ephemeral", label: t("workspaces.create.dataPolicyEphemeral") },
  ];

  return (
    <Page
      title={t("workspaces.create.title")}
      width="narrow"
      eyebrow={<Link to="/">{t("nav.allWorkspaces")}</Link>}
    >
      <ErrorBanner error={templates.error ?? error} onDismiss={() => setError(null)} />
      {templates.loading && !templates.data ? (
        <Spinner label={t("workspaces.create.loading")} />
      ) : (
        <form onSubmit={(e) => void submit(e)}>
          <Input
            label={t("workspaces.create.nameLabel")}
            name="name"
            required
            pattern={NAME_PATTERN}
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={t("workspaces.create.namePlaceholder")}
            autoComplete="off"
          />
          <Select
            label={t("workspaces.create.templateLabel")}
            name="template"
            required
            value={templateRef}
            onChange={(e) => setTemplateRef(e.target.value)}
          >
            <option value="" disabled>
              {t("workspaces.create.templatePlaceholder")}
            </option>
            {(templates.data ?? []).map((tpl) => (
              <option key={tpl.id} value={tpl.id}>
                {t("workspaces.create.templateOption", {
                  name: tpl.name,
                  revision: tpl.revision,
                  runtime: tpl.runtime,
                })}
              </option>
            ))}
          </Select>
          {selected?.networkProfile ? (
            <p className="tc-field__hint">
              {t("workspaces.create.network", {
                profile: networkProfileLabel(selected.networkProfile),
              })}
            </p>
          ) : null}
          <Select
            label={t("workspaces.create.dataPolicyLabel")}
            name="dataPolicy"
            value={dataPolicy}
            onChange={(e) => setDataPolicy(e.target.value as PolicyChoice)}
            options={policyOptions}
          />
          {effectivePolicy === "Ephemeral" ? (
            <Alert tone="warning" title={t("workspaces.create.ephemeral.title")}>
              {t("workspaces.create.ephemeral.body")}
            </Alert>
          ) : null}
          <Checkbox
            name="startNow"
            label={t("workspaces.create.startNow")}
            checked={startNow}
            onChange={(e) => setStartNow(e.target.checked)}
          />
          <Button
            type="submit"
            variant="primary"
            loading={busy}
            disabled={!name.trim() || !templateRef}
          >
            {busy ? t("workspaces.create.submitting") : t("workspaces.create.submit")}
          </Button>
        </form>
      )}
    </Page>
  );
}
