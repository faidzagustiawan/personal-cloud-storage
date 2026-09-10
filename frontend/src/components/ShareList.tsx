import { Link } from "react-router-dom";

import { formatDate } from "../lib/format";
import { useRevokeShare, useShares } from "./ShareDialog";
import { Notice, Spinner } from "./ui";

/**
 * Every link handed out, in one place.
 *
 * A share is a standing grant to someone with no account, so being able to see
 * what is currently open — and how often it has been fetched — is the point of
 * the list, not a nicety.
 */
export function ShareList() {
  const shares = useShares();
  const revoke = useRevokeShare();

  if (shares.isPending) return <Spinner className="text-ink-3" />;
  if (shares.error) return <Notice>{shares.error.message}</Notice>;

  const items = shares.data ?? [];
  const active = items.filter((share) => share.active);
  const past = items.filter((share) => !share.active);

  if (items.length === 0) {
    return (
      <p className="text-sm text-ink-3">
        No links yet. Open a photo and choose Share to create one.
      </p>
    );
  }

  return (
    <div className="flex flex-col gap-3">
      <ul className="flex flex-col gap-1.5">
        {active.map((share) => (
          <li
            key={share.id}
            className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-sm border border-line bg-surface px-3 py-2"
          >
            <div className="flex min-w-0 flex-1 flex-col">
              <Link
                to={`/?file=${share.file_id}`}
                className="truncate text-sm text-ink hover:text-accent"
                title={share.filename}
              >
                {share.filename}
              </Link>
              <span className="tabular text-[11px] text-ink-3">
                Expires {formatDate(share.expires_at)} · {share.view_count} view
                {share.view_count === 1 ? "" : "s"}
              </span>
            </div>

            <button
              type="button"
              onClick={() => revoke.mutate(share.id)}
              disabled={revoke.isPending}
              className="text-xs font-medium text-danger underline-offset-2 hover:underline disabled:opacity-55"
            >
              Revoke
            </button>
          </li>
        ))}
      </ul>

      {revoke.error && <Notice>{revoke.error.message}</Notice>}

      {past.length > 0 && (
        <p className="tabular text-[11px] text-ink-3">
          {past.length} link{past.length === 1 ? "" : "s"} already expired or revoked.
        </p>
      )}
    </div>
  );
}
