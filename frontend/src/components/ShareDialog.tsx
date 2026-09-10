import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { api } from "../lib/api";
import { formatDate } from "../lib/format";
import type { FileItem, Share } from "../lib/types";
import { Button, Notice, Spinner } from "./ui";

const shareKeys = { all: ["shares"] as const };

export function useShares() {
  return useQuery({ queryKey: shareKeys.all, queryFn: async () => (await api.listShares()).items ?? [] });
}

export function useRevokeShare() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.revokeShare(id),
    onSuccess: () => client.invalidateQueries({ queryKey: shareKeys.all }),
  });
}

const DURATIONS = [1, 7, 30] as const;

/**
 * Creates or shows the link for one file.
 *
 * Anyone holding the link can fetch the file with no account, so the dialog
 * states the expiry up front and keeps revoking one click away.
 */
export function ShareDialog({ file, onClose }: { file: FileItem; onClose: () => void }) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const client = useQueryClient();
  const shares = useShares();
  const revoke = useRevokeShare();
  const [days, setDays] = useState<number>(7);
  const [copied, setCopied] = useState(false);

  const closeRef = useRef(onClose);
  closeRef.current = onClose;

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (!dialog.open) dialog.showModal();

    // Escape is intercepted so React stays the single source of truth for
    // whether this dialog exists; see Viewer.tsx for the full reasoning.
    const requestClose = (event?: Event) => {
      event?.preventDefault();
      closeRef.current();
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") requestClose(event);
    };
    dialog.addEventListener("keydown", onKeyDown);
    dialog.addEventListener("cancel", requestClose);

    return () => {
      dialog.removeEventListener("keydown", onKeyDown);
      dialog.removeEventListener("cancel", requestClose);
      if (dialog.open) dialog.close();
    };
  }, []);

  const create = useMutation({
    mutationFn: () => api.createShare(file.id, days),
    onSuccess: () => client.invalidateQueries({ queryKey: shareKeys.all }),
  });

  const active = (shares.data ?? []).filter((s) => s.file_id === file.id && s.active);

  return (
    <dialog
      ref={dialogRef}
      onClick={(event) => {
        if (event.target === dialogRef.current) onClose();
      }}
      className="w-[min(30rem,calc(100vw-2rem))] rounded-sm border border-line bg-surface p-0 text-ink backdrop:bg-black/60"
    >
      <div className="flex flex-col gap-4 p-4">
        <header className="flex items-baseline gap-3">
          <h2 className="text-sm font-semibold text-ink">Share this file</h2>
          <span className="min-w-0 flex-1 truncate text-xs text-ink-3">{file.filename}</span>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            className="text-ink-3 hover:text-ink"
          >
            <svg width="14" height="14" viewBox="0 0 14 14" aria-hidden="true">
              <path d="M3 3l8 8M11 3l-8 8" stroke="currentColor" strokeWidth="1.6" fill="none" />
            </svg>
          </button>
        </header>

        {shares.isPending ? (
          <Spinner className="text-ink-3" />
        ) : active.length > 0 ? (
          <div className="flex flex-col gap-2">
            {active.map((share) => (
              <ActiveLink
                key={share.id}
                share={share}
                copied={copied}
                onCopy={() => {
                  copyToClipboard(share.url);
                  setCopied(true);
                  window.setTimeout(() => setCopied(false), 1800);
                }}
                onRevoke={() => revoke.mutate(share.id)}
                revoking={revoke.isPending}
              />
            ))}
          </div>
        ) : (
          <div className="flex flex-col gap-3">
            <fieldset className="flex flex-col gap-1.5">
              <legend className="text-[11px] font-semibold tracking-wide text-ink-2 uppercase">
                Link expires after
              </legend>
              <div className="flex rounded-sm border border-line">
                {DURATIONS.map((option, i) => (
                  <button
                    key={option}
                    type="button"
                    onClick={() => setDays(option)}
                    aria-pressed={days === option}
                    className={`flex-1 px-3 py-1.5 text-sm transition ${i > 0 ? "border-l border-line" : ""} ${
                      days === option ? "bg-accent-soft font-semibold text-ink" : "text-ink-2 hover:bg-surface-2"
                    }`}
                  >
                    {option === 1 ? "1 day" : `${option} days`}
                  </button>
                ))}
              </div>
            </fieldset>

            <p className="text-xs leading-relaxed text-ink-3">
              Anyone with the link can view and download this file without signing in. You can
              revoke it at any time and it stops working immediately.
            </p>

            {create.error && <Notice>{create.error.message}</Notice>}

            <Button onClick={() => create.mutate()} loading={create.isPending}>
              Create link
            </Button>
          </div>
        )}

        {revoke.error && <Notice>{revoke.error.message}</Notice>}
      </div>
    </dialog>
  );
}

function ActiveLink({
  share,
  copied,
  onCopy,
  onRevoke,
  revoking,
}: {
  share: Share;
  copied: boolean;
  onCopy: () => void;
  onRevoke: () => void;
  revoking: boolean;
}) {
  return (
    <div className="flex flex-col gap-2 rounded-sm border border-line bg-surface-2 p-2.5">
      <div className="flex items-center gap-2">
        <input
          readOnly
          value={share.url}
          onFocus={(event) => event.target.select()}
          className="tabular min-w-0 flex-1 rounded-sm border border-line bg-surface px-2 py-1.5 text-xs text-ink"
        />
        <Button onClick={onCopy}>{copied ? "Copied" : "Copy"}</Button>
      </div>

      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-ink-3">
        <span className="tabular">Expires {formatDate(share.expires_at)}</span>
        <span className="tabular">
          {share.view_count} view{share.view_count === 1 ? "" : "s"}
        </span>
        <div className="flex-1" />
        <button
          type="button"
          onClick={onRevoke}
          disabled={revoking}
          className="font-medium text-danger underline-offset-2 hover:underline disabled:opacity-55"
        >
          Revoke
        </button>
      </div>
    </div>
  );
}

/**
 * navigator.clipboard needs a secure context, which plain-HTTP local
 * development is not, so the old selection trick is kept as a fallback.
 */
function copyToClipboard(text: string): void {
  if (navigator.clipboard?.writeText) {
    void navigator.clipboard.writeText(text).catch(() => fallbackCopy(text));
    return;
  }
  fallbackCopy(text);
}

function fallbackCopy(text: string): void {
  const field = document.createElement("textarea");
  field.value = text;
  field.setAttribute("readonly", "");
  field.style.position = "fixed";
  field.style.opacity = "0";
  document.body.appendChild(field);
  field.select();
  try {
    document.execCommand("copy");
  } catch {
    // Nothing further to try; the field is selectable in the dialog itself.
  }
  document.body.removeChild(field);
}
