import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "../lib/api";
import { formatDateTime } from "../lib/format";
import { Button, Notice, Spinner } from "./ui";

const sessionKeys = { all: ["sessions"] as const };

/**
 * Signed-in devices, and a way to cut one off.
 *
 * This is what opaque database-backed sessions buy over a stateless token
 * (decision D3): a session left open on a borrowed laptop can be ended without
 * changing the password and signing out everything else.
 */
export function SessionList() {
  const client = useQueryClient();
  const sessions = useQuery({
    queryKey: sessionKeys.all,
    queryFn: async () => (await api.listSessions()).items ?? [],
  });

  const revoke = useMutation({
    mutationFn: (id: string) => api.revokeSession(id),
    onSuccess: (result) => {
      if (result.signed_out) {
        // We just ended our own session; the cookie is already cleared, so the
        // next request will redirect to the sign-in page.
        client.clear();
        window.location.assign("/login");
        return;
      }
      client.invalidateQueries({ queryKey: sessionKeys.all });
    },
  });

  const revokeOthers = useMutation({
    mutationFn: () => api.revokeOtherSessions(),
    onSuccess: () => client.invalidateQueries({ queryKey: sessionKeys.all }),
  });

  if (sessions.isPending) return <Spinner className="text-ink-3" />;
  if (sessions.error) return <Notice>{sessions.error.message}</Notice>;

  const items = sessions.data ?? [];
  const others = items.filter((session) => !session.current).length;

  return (
    <div className="flex flex-col gap-2">
      <ul className="flex flex-col gap-1.5">
        {items.map((session) => (
          <li
            key={session.id}
            className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-sm border border-line bg-surface px-3 py-2"
          >
            <div className="flex min-w-0 flex-1 flex-col">
              <span className="truncate text-sm text-ink" title={session.user_agent}>
                {describeAgent(session.user_agent)}
                {session.current && (
                  <span className="ml-2 rounded-xs bg-accent-soft px-1.5 py-0.5 text-[10px] font-semibold tracking-wide text-ink uppercase">
                    This device
                  </span>
                )}
              </span>
              <span className="tabular text-[11px] text-ink-3">
                Signed in {formatDateTime(session.created_at)}
              </span>
            </div>

            <button
              type="button"
              onClick={() => revoke.mutate(session.id)}
              disabled={revoke.isPending}
              className="text-xs font-medium text-danger underline-offset-2 hover:underline disabled:opacity-55"
            >
              {session.current ? "Sign out" : "End session"}
            </button>
          </li>
        ))}
      </ul>

      {revoke.error && <Notice>{revoke.error.message}</Notice>}
      {revokeOthers.error && <Notice>{revokeOthers.error.message}</Notice>}

      {others > 0 && (
        <Button
          variant="ghost"
          loading={revokeOthers.isPending}
          onClick={() => revokeOthers.mutate()}
          className="self-start"
        >
          End {others} other session{others === 1 ? "" : "s"}
        </Button>
      )}
    </div>
  );
}

/**
 * A raw user agent string is unreadable, and the point of this list is to let
 * someone recognise their own devices.
 */
function describeAgent(agent: string): string {
  if (!agent) return "Unknown device";

  const platform = /iPhone/.test(agent)
    ? "iPhone"
    : /iPad/.test(agent)
      ? "iPad"
      : /Android/.test(agent)
        ? "Android"
        : /Macintosh|Mac OS X/.test(agent)
          ? "Mac"
          : /Windows/.test(agent)
            ? "Windows"
            : /Linux/.test(agent)
              ? "Linux"
              : "Unknown device";

  // Order matters: Chrome and Edge both claim Safari, and Edge claims Chrome.
  const browser = /Edg\//.test(agent)
    ? "Edge"
    : /Firefox\//.test(agent)
      ? "Firefox"
      : /Chrome\//.test(agent)
        ? "Chrome"
        : /Safari\//.test(agent)
          ? "Safari"
          : "";

  return browser ? `${platform} · ${browser}` : platform;
}
