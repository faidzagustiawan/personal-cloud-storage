import { type FormEvent, useState } from "react";
import { NavLink, useNavigate, useParams } from "react-router-dom";

import { useCreateFolder, useFolders } from "../hooks/queries";
import { useDeleteFolder, useRenameFolder } from "../hooks/useFiles";
import type { Folder } from "../lib/types";
import { Notice, Spinner } from "./ui";

export function FolderList({ onNavigate }: { onNavigate: () => void }) {
  const folders = useFolders();
  const create = useCreateFolder();
  const [adding, setAdding] = useState(false);
  const [name, setName] = useState("");

  function submit(event: FormEvent) {
    event.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) return;
    create.mutate(trimmed, {
      onSuccess: () => {
        setName("");
        setAdding(false);
      },
    });
  }

  return (
    <section className="flex flex-col gap-1.5">
      <div className="flex items-center gap-1">
        <h2 className="text-[11px] font-semibold tracking-[0.12em] text-ink-3 uppercase">
          Folders
        </h2>
        <div className="flex-1" />
        <button
          type="button"
          onClick={() => setAdding((value) => !value)}
          aria-label="New folder"
          className="rounded-xs px-1 text-ink-3 hover:text-ink"
        >
          <svg width="13" height="13" viewBox="0 0 13 13" aria-hidden="true">
            <path d="M6.5 2v9M2 6.5h9" stroke="currentColor" strokeWidth="1.6" fill="none" />
          </svg>
        </button>
      </div>

      {adding && (
        <form onSubmit={submit} className="flex flex-col gap-1">
          <input
            autoFocus
            value={name}
            onChange={(event) => setName(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Escape") setAdding(false);
            }}
            placeholder="Folder name"
            maxLength={64}
            className="rounded-sm border border-line bg-surface px-2 py-1 text-sm"
          />
          {create.error && <Notice>{create.error.message}</Notice>}
        </form>
      )}

      {folders.isPending && <Spinner className="text-ink-3" />}
      {folders.data?.length === 0 && !adding && (
        <p className="text-xs text-ink-3">No folders yet.</p>
      )}

      {folders.data?.map((folder) => (
        <FolderRow key={folder.id} folder={folder} onNavigate={onNavigate} />
      ))}
    </section>
  );
}

function FolderRow({ folder, onNavigate }: { folder: Folder; onNavigate: () => void }) {
  const rename = useRenameFolder();
  const remove = useDeleteFolder();
  const navigate = useNavigate();
  const params = useParams();

  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(folder.name);
  const [confirming, setConfirming] = useState(false);

  const isOpen = params.folderId === String(folder.id);

  function leaveIfOpen() {
    if (isOpen) navigate("/");
  }

  if (editing) {
    return (
      <form
        className="flex flex-col gap-1"
        onSubmit={(event) => {
          event.preventDefault();
          rename.mutate(
            { id: folder.id, name: name.trim() },
            { onSuccess: () => setEditing(false) },
          );
        }}
      >
        <input
          autoFocus
          value={name}
          onChange={(event) => setName(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Escape") {
              setName(folder.name);
              setEditing(false);
            }
          }}
          maxLength={64}
          className="rounded-sm border border-line bg-surface px-2 py-1 text-sm"
        />
        {rename.error && <Notice>{rename.error.message}</Notice>}
      </form>
    );
  }

  return (
    <div className="group flex flex-col">
      <div className="flex items-center gap-1">
        <NavLink
          to={`/folder/${folder.id}`}
          onClick={onNavigate}
          className={({ isActive }) =>
            `flex min-w-0 flex-1 items-center gap-2 rounded-sm px-2 py-1.5 text-sm transition ${
              isActive ? "bg-accent-soft font-semibold text-ink" : "text-ink-2 hover:bg-surface-2"
            }`
          }
        >
          <span className="truncate">{folder.name}</span>
          <span className="tabular ml-auto text-xs text-ink-3">{folder.file_count}</span>
        </NavLink>

        <div className="flex opacity-0 transition group-focus-within:opacity-100 group-hover:opacity-100">
          <IconButton label={`Rename ${folder.name}`} onClick={() => setEditing(true)}>
            <path d="M2 11h2.5l6-6L8 2.5l-6 6V11Z" fill="none" stroke="currentColor" strokeWidth="1.3" />
          </IconButton>
          <IconButton label={`Delete ${folder.name}`} onClick={() => setConfirming(true)}>
            <path d="M2.5 3.5h8M5 3.5V2.5h3v1M4 3.5l.5 7h4l.5-7" fill="none" stroke="currentColor" strokeWidth="1.3" />
          </IconButton>
        </div>
      </div>

      {/* Deleting a folder full of photos is not obviously the same request as
          deleting the folder, so the choice is stated rather than assumed. */}
      {confirming && (
        <div className="mt-1 flex flex-col gap-1.5 rounded-sm border border-line bg-surface-2 p-2">
          <p className="text-xs text-ink-2">
            {folder.file_count === 0
              ? `Delete “${folder.name}”?`
              : `“${folder.name}” holds ${folder.file_count} item${folder.file_count === 1 ? "" : "s"}.`}
          </p>
          <div className="flex flex-wrap gap-1.5">
            {folder.file_count > 0 && (
              <SmallButton
                onClick={() =>
                  remove.mutate(
                    { id: folder.id, mode: "move_to_root" },
                    { onSuccess: () => { setConfirming(false); leaveIfOpen(); } },
                  )
                }
              >
                Keep photos
              </SmallButton>
            )}
            <SmallButton
              tone="danger"
              onClick={() =>
                remove.mutate(
                  { id: folder.id, ...(folder.file_count > 0 ? { mode: "cascade" as const } : {}) },
                  { onSuccess: () => { setConfirming(false); leaveIfOpen(); } },
                )
              }
            >
              {folder.file_count > 0 ? "Trash photos too" : "Delete"}
            </SmallButton>
            <SmallButton onClick={() => setConfirming(false)}>Cancel</SmallButton>
          </div>
          {folder.file_count > 0 && (
            <p className="text-[11px] text-ink-3">
              Trashed photos stay recoverable for 30 days.
            </p>
          )}
          {remove.error && <Notice>{remove.error.message}</Notice>}
        </div>
      )}
    </div>
  );
}

function IconButton({
  label,
  onClick,
  children,
}: {
  label: string;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={label}
      className="rounded-xs p-1 text-ink-3 hover:text-ink"
    >
      <svg width="13" height="13" viewBox="0 0 13 13" aria-hidden="true">
        {children}
      </svg>
    </button>
  );
}

function SmallButton({
  tone = "plain",
  onClick,
  children,
}: {
  tone?: "plain" | "danger";
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded-sm px-2 py-1 text-xs font-medium transition ${
        tone === "danger"
          ? "bg-danger-soft text-danger hover:brightness-105"
          : "border border-line text-ink-2 hover:text-ink"
      }`}
    >
      {children}
    </button>
  );
}
