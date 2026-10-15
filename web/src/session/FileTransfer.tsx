import { useRef, useState } from "react";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { unwrap } from "../api/client";
import { isPortalApiError } from "../api/errors";
import { Button } from "../design/Button";
import { Input } from "../design/Field";
import { useToast } from "../design/Toast";
import {
  TransferError,
  checkedContentUrl,
  getTransferContent,
  putTransferContent,
  saveBlob,
  transferDirections,
  validTransferName,
  type FileTransferPolicy,
} from "./transfer";

export interface FileTransferProps {
  workspaceId: string;
  sessionDomain: string;
  policy: FileTransferPolicy;
  maxFileBytes: number;
}

type MessageKey = Parameters<typeof t>[0];

function failureKey(err: unknown): MessageKey {
  if (err instanceof TransferError) {
    switch (err.code) {
      case "transfer_denied":
        return "session.transfer.error.denied";
      case "transfer_busy":
        return "session.transfer.error.busy";
      case "too_large":
        return "session.transfer.error.tooLarge";
      case "file_not_found":
        return "session.transfer.error.notFound";
      case "transfer_invalid":
        return "session.transfer.error.expired";
      case "rate_limited":
      case "too_many_streams":
        return "session.transfer.error.rateLimited";
      case "unauthorized":
        return "session.transfer.error.session";
    }
    return "session.transfer.error.generic";
  }
  if (isPortalApiError(err)) {
    if (err.httpStatus === 400) return "session.transfer.error.badName";
    if (err.httpStatus === 403) return "session.transfer.error.denied";
    if (err.httpStatus === 409) return "session.transfer.error.notRunning";
    if (err.httpStatus === 429) return "session.transfer.error.rateLimited";
  }
  return "session.transfer.error.generic";
}

function mib(bytes: number): string {
  return (bytes / 1048576).toFixed(bytes % 1048576 === 0 ? 0 : 1);
}

export function FileTransfer({ workspaceId, sessionDomain, policy, maxFileBytes }: FileTransferProps) {
  const api = useApi();
  const { toast } = useToast();
  const fileInput = useRef<HTMLInputElement>(null);
  const [busy, setBusy] = useState<"upload" | "download" | null>(null);
  const [name, setName] = useState("");
  const dirs = transferDirections(policy);
  if (!dirs.upload && !dirs.download) return null;

  const mint = async (direction: "upload" | "download", path: string, bytes?: number) => {
    const grant = unwrap(
      await api.POST("/v1/workspaces/{workspaceId}/file-transfers", {
        params: { path: { workspaceId } },
        body: { direction, path, ...(bytes === undefined ? {} : { bytes }) },
      }),
    );
    return checkedContentUrl(grant.contentUrl, workspaceId, sessionDomain);
  };

  const upload = async (file: File) => {
    if (!validTransferName(file.name)) {
      toast({ tone: "danger", title: t("session.transfer.error.badName") });
      return;
    }
    if (file.size > maxFileBytes) {
      toast({ tone: "danger", title: t("session.transfer.error.tooLarge"), description: t("session.transfer.limit", { mib: mib(maxFileBytes) }) });
      return;
    }
    setBusy("upload");
    try {
      const url = await mint("upload", file.name, file.size);
      await putTransferContent(url, file);
      toast({ tone: "success", title: t("session.transfer.uploaded", { name: file.name }) });
    } catch (err) {
      toast({ tone: "danger", title: t(failureKey(err)) });
    } finally {
      setBusy(null);
      if (fileInput.current) fileInput.current.value = "";
    }
  };

  const download = async () => {
    const wanted = name.trim();
    if (!validTransferName(wanted)) {
      toast({ tone: "danger", title: t("session.transfer.error.badName") });
      return;
    }
    setBusy("download");
    try {
      const url = await mint("download", wanted);
      saveBlob(await getTransferContent(url), wanted);
      toast({ tone: "success", title: t("session.transfer.downloaded", { name: wanted }) });
    } catch (err) {
      toast({ tone: "danger", title: t(failureKey(err)) });
    } finally {
      setBusy(null);
    }
  };

  return (
    <section className="tc-session__transfer" aria-label={t("session.transfer.region")}>
      <p className="tc-session__transfer-hint">
        {t("session.transfer.folderHint")} {t("session.transfer.limit", { mib: mib(maxFileBytes) })}
      </p>
      {dirs.upload ? (
        <>
          <input
            ref={fileInput}
            type="file"
            className="tc-sr-only"
            aria-label={t("session.transfer.uploadInput")}
            onChange={(e) => {
              const f = e.currentTarget.files?.[0];
              if (f) void upload(f);
            }}
          />
          <Button size="sm" variant="secondary" loading={busy === "upload"} disabled={busy !== null} onClick={() => fileInput.current?.click()}>
            {t("session.transfer.upload")}
          </Button>
        </>
      ) : null}
      {dirs.download ? (
        <form
          className="tc-session__transfer-download"
          onSubmit={(e) => {
            e.preventDefault();
            void download();
          }}
        >
          <Input
            label={t("session.transfer.downloadName")}
            hideLabel
            value={name}
            placeholder={t("session.transfer.downloadPlaceholder")}
            onChange={(e) => setName(e.currentTarget.value)}
          />
          <Button size="sm" variant="secondary" type="submit" loading={busy === "download"} disabled={busy !== null || name.trim() === ""}>
            {t("session.transfer.download")}
          </Button>
        </form>
      ) : null}
    </section>
  );
}
