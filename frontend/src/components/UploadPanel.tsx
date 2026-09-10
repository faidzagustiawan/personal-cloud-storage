import { useState } from "react";

import { formatBytes } from "../lib/format";
import { uploadQueue, type TaskState, type UploadTask } from "../upload/queue";
import { useUploadTasks } from "../upload/useUploads";

/**
 * The upload panel outlives navigation on purpose: starting a 400 MB video and
 * then opening a folder should not hide the fact that it is still going.
 */
export function UploadPanel() {
  const tasks = useUploadTasks();
  const [collapsed, setCollapsed] = useState(false);

  if (tasks.length === 0) return null;

  const active = tasks.filter((t) => t.state !== "done" && t.state !== "cancelled" && t.state !== "error");
  const failed = tasks.filter((t) => t.state === "error");
  const allSettled = active.length === 0;

  return (
    <section
      aria-label="Uploads"
      className="fixed right-4 bottom-4 z-20 flex w-[min(22rem,calc(100vw-2rem))] flex-col overflow-hidden rounded-sm border border-line bg-surface shadow-lg"
    >
      <header className="flex items-center gap-2 border-b border-line px-3 py-2">
        <h2 className="text-sm font-semibold text-ink">
          {allSettled
            ? `${tasks.length} upload${tasks.length === 1 ? "" : "s"} finished`
            : `Uploading ${active.length} file${active.length === 1 ? "" : "s"}`}
        </h2>
        {failed.length > 0 && (
          <span className="tabular rounded-sm bg-danger-soft px-1.5 py-0.5 text-[10px] font-bold text-danger">
            {failed.length} failed
          </span>
        )}

        <div className="flex-1" />

        <button
          type="button"
          onClick={() => setCollapsed((v) => !v)}
          className="px-1 text-xs text-ink-2 hover:text-ink"
          aria-expanded={!collapsed}
        >
          {collapsed ? "Show" : "Hide"}
        </button>
        {allSettled && (
          <button
            type="button"
            onClick={() => uploadQueue.clearFinished()}
            className="px-1 text-xs text-ink-2 hover:text-ink"
          >
            Clear
          </button>
        )}
      </header>

      {!collapsed && (
        <ul className="max-h-72 overflow-y-auto">
          {tasks.map((task) => (
            <Row key={task.id} task={task} />
          ))}
        </ul>
      )}
    </section>
  );
}

function Row({ task }: { task: UploadTask }) {
  const percent = task.size > 0 ? Math.min(100, (task.sent / task.size) * 100) : 0;
  const busy = task.state === "preparing" || task.state === "uploading" || task.state === "finishing";

  return (
    <li className="flex flex-col gap-1 border-b border-line-soft px-3 py-2 last:border-b-0">
      <div className="flex items-baseline gap-2">
        <span className="min-w-0 flex-1 truncate text-xs text-ink" title={task.name}>
          {task.name}
        </span>
        <span className="tabular shrink-0 text-[11px] text-ink-3">{label(task)}</span>
      </div>

      {busy && (
        <div className="h-0.5 w-full overflow-hidden rounded-full bg-line">
          <div
            className={`h-full ${task.state === "uploading" ? "bg-accent" : "bg-ink-3"}`}
            style={{ width: task.state === "uploading" ? `${percent}%` : "100%" }}
          />
        </div>
      )}

      {task.error && <p className="text-[11px] text-danger">{task.error}</p>}

      {/* Saying a thumbnail is queued matters: the grid will show a placeholder
          for a while, and silence would read as a broken upload. */}
      {task.state === "done" && task.thumbnailQueued && (
        <p className="text-[11px] text-ink-3">Stored. Preview will follow shortly.</p>
      )}

      {busy && (
        <button
          type="button"
          onClick={() => uploadQueue.cancel(task.id)}
          className="self-start text-[11px] text-ink-3 underline-offset-2 hover:text-danger hover:underline"
        >
          Cancel
        </button>
      )}
    </li>
  );
}

function label(task: UploadTask): string {
  const states: Record<TaskState, string> = {
    queued: "Waiting",
    preparing: "Preparing",
    uploading: `${formatBytes(task.sent)} / ${formatBytes(task.size)}`,
    finishing: "Finishing",
    done: "Done",
    error: "Failed",
    cancelled: "Cancelled",
  };
  return states[task.state];
}
