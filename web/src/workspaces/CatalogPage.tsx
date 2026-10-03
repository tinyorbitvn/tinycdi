import { Alert, Badge, Button, buttonClass, Card, DescriptionList, EmptyState, Grid, Page, Spinner } from "../design";
import { IconGrid, IconPlus } from "../design/icons";
import { t } from "../i18n";
import { isPortalApiError } from "../api/errors";
import { Link } from "../lib/router";
import { useTemplates } from "../templates/useTemplates";
import { TemplateIcon } from "../templates/TemplateIcon";
import {
  clipboardPolicyLabel,
  dataPolicyLabel,
  experienceLabel,
  formatDate,
  formatDuration,
  formatResources,
  networkProfileLabel,
  runtimeLabel,
} from "../templates/format";
import type { TemplateView } from "../templates/types";
import { ErrorBanner } from "./ErrorBanner";

/** Warning badge for a template whose runtime image is stale. */
function StaleBadge() {
  return <Badge tone="warning">{t("templates.catalog.stale.badge")}</Badge>;
}

/** Danger badge for a template whose runtime image is over the block limit. */
function BlockedBadge() {
  return <Badge tone="danger">{t("templates.catalog.blocked.badge")}</Badge>;
}

export function TemplateCard({ template }: { template: TemplateView }) {
  const details = [
    { term: t("templates.catalog.field.runtime"), detail: runtimeLabel(template.runtime) },
    { term: t("templates.catalog.field.experience"), detail: experienceLabel(template.experience) },
    { term: t("templates.catalog.field.resources"), detail: formatResources(template.resources) },
    {
      term: t("templates.catalog.field.dataPolicy"),
      detail: dataPolicyLabel(template.dataPolicyDefault),
    },
    { term: t("templates.catalog.field.clipboard"), detail: clipboardPolicyLabel(template.clipboardPolicy) },
    ...(template.networkProfile
      ? [{ term: t("templates.catalog.field.network"), detail: networkProfileLabel(template.networkProfile) }]
      : []),
    {
      term: t("templates.catalog.field.idleTimeout"),
      detail: formatDuration(template.lifecycleDefaults.idleTimeoutSeconds),
    },
  ];
  return (
    <Card
      as="article"
      headingLevel={3}
      title={
        <>
          <TemplateIcon runtime={template.runtime} experience={template.experience} /> {template.name}
        </>
      }
      description={template.description}
      actions={
        template.imageBlocked === true ? (
          <BlockedBadge />
        ) : template.imageStale === true ? (
          <StaleBadge />
        ) : undefined
      }
      footer={
        template.imageBlocked === true ? (
          <Button variant="primary" size="sm" disabled>
            <IconPlus size={16} /> {t("templates.catalog.create")}
          </Button>
        ) : (
          <Link to={`/workspaces/new?template=${template.id}`} className={buttonClass("primary", "sm")}>
            <IconPlus size={16} /> {t("templates.catalog.create")}
          </Link>
        )
      }
    >
      {template.imageBlocked === true ? (
        <Alert tone="danger">
          {template.imageBuiltAt
            ? t("templates.catalog.blocked.hint", { date: formatDate(template.imageBuiltAt) })
            : t("templates.catalog.blocked.badge")}
        </Alert>
      ) : template.imageStale === true ? (
        <Alert tone="warning">
          {template.imageBuiltAt
            ? t("templates.catalog.stale.hint", { date: formatDate(template.imageBuiltAt) })
            : t("templates.catalog.stale.badge")}
        </Alert>
      ) : null}
      <DescriptionList items={details} />
    </Card>
  );
}

export function CatalogPage() {
  const list = useTemplates();

  if (isPortalApiError(list.error) && list.error.code === "UNAUTHENTICATED") {
    window.location.assign("/v1/login");
    return null;
  }

  return (
    <Page title={t("templates.catalog.title")}>
      <ErrorBanner error={list.error} onRetry={() => void list.refresh()} onDismiss={list.clearError} />
      {list.loading && !list.data ? (
        <Spinner label={t("templates.catalog.loading")} />
      ) : (list.data ?? []).length === 0 ? (
        <EmptyState icon={<IconGrid size={24} />} title={t("templates.catalog.empty")} />
      ) : (
        <Grid min="md">
          {(list.data ?? []).map((tpl) => (
            <TemplateCard key={`${tpl.id}@${tpl.revision}`} template={tpl} />
          ))}
        </Grid>
      )}
    </Page>
  );
}
