import { type ReactNode, useState } from "react";
import { NavLink, Outlet, useNavigate } from "react-router-dom";

import { useLogout, useMe, useUsage } from "../hooks/queries";
import { formatBytes } from "../lib/format";
import { useUploadRefresh } from "../upload/useUploads";
import { FolderList } from "./FolderList";
import { Button } from "./ui";
import { UploadPanel } from "./UploadPanel";

export function AppLayout() {
  const { data: me } = useMe();
  const [navOpen, setNavOpen] = useState(false);

  // Committed uploads refresh the gallery and the storage meter.
  useUploadRefresh();

  return (
    <div className="flex h-full min-h-0 flex-col">
      <TopBar username={me?.username ?? ""} onToggleNav={() => setNavOpen((v) => !v)} />

      <div className="flex min-h-0 flex-1 items-stretch">
        {/* The sidebar collapses below the phone breakpoint rather than
            shrinking: on a phone the gallery should own the full width. */}
        <aside
          className={`${navOpen ? "block" : "hidden"} w-full shrink-0 border-r border-line bg-surface md:block md:w-60`}
        >
          <Sidebar onNavigate={() => setNavOpen(false)} />
        </aside>

        <main className={`${navOpen ? "hidden" : "flex"} min-w-0 flex-1 flex-col md:flex`}>
          <Outlet />
        </main>
      </div>

      <UploadPanel />
    </div>
  );
}

function TopBar({ username, onToggleNav }: { username: string; onToggleNav: () => void }) {
  const logout = useLogout();
  const navigate = useNavigate();

  return (
    <header className="sticky top-0 z-10 flex items-center gap-3 border-b border-line bg-surface px-4 py-2.5">
      <button
        type="button"
        onClick={onToggleNav}
        aria-label="Toggle navigation"
        className="rounded-sm border border-line px-2 py-1 text-ink-2 md:hidden"
      >
        <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
          <path d="M2 4h12M2 8h12M2 12h12" stroke="currentColor" strokeWidth="1.5" fill="none" />
        </svg>
      </button>

      <span className="tabular text-[11px] tracking-[0.16em] text-ink-3 uppercase">cloud</span>

      <div className="flex-1" />

      <span className="hidden text-sm text-ink-2 sm:inline">{username}</span>
      <Button
        variant="ghost"
        loading={logout.isPending}
        onClick={() => logout.mutate(undefined, { onSettled: () => navigate("/login") })}
      >
        Sign out
      </Button>
    </header>
  );
}

function Sidebar({ onNavigate }: { onNavigate: () => void }) {
  const usage = useUsage();

  return (
    <nav className="flex h-full flex-col gap-6 overflow-y-auto p-4">
      <div className="flex flex-col gap-0.5">
        <SidebarLink to="/" end onNavigate={onNavigate}>
          All photos
        </SidebarLink>
        <SidebarLink to="/trash" onNavigate={onNavigate}>
          Trash
        </SidebarLink>
        <SidebarLink to="/settings" onNavigate={onNavigate}>
          Settings
        </SidebarLink>
      </div>

      <FolderList onNavigate={onNavigate} />

      <div className="mt-auto">{usage.data && <StorageMeter usage={usage.data} />}</div>
    </nav>
  );
}

function SidebarLink({
  to,
  end,
  onNavigate,
  children,
}: {
  to: string;
  end?: boolean;
  onNavigate: () => void;
  children: ReactNode;
}) {
  return (
    <NavLink
      to={to}
      end={end}
      onClick={onNavigate}
      className={({ isActive }) =>
        `flex items-center gap-2 rounded-sm px-2 py-1.5 text-sm transition ${
          isActive ? "bg-accent-soft font-semibold text-ink" : "text-ink-2 hover:bg-surface-2"
        }`
      }
    >
      {children}
    </NavLink>
  );
}

function StorageMeter({
  usage,
}: {
  usage: { used_bytes: number; quota_bytes: number; usage_percent: number; thumbnails_pending: number };
}) {
  const percent = Math.min(100, Math.max(0, usage.usage_percent));
  // Storage cost is linear and unbounded (decision D9), so the meter is a real
  // number the user is meant to act on, not decoration.
  const tone = percent > 90 ? "bg-danger" : percent > 75 ? "bg-accent" : "bg-ink-3";

  return (
    <section className="flex flex-col gap-1.5" aria-label="Storage usage">
      <div className="h-1 w-full overflow-hidden rounded-full bg-line">
        <div className={`h-full ${tone}`} style={{ width: `${percent}%` }} />
      </div>
      <p className="tabular text-xs text-ink-2">
        {formatBytes(usage.used_bytes)} of {formatBytes(usage.quota_bytes)}
      </p>
      {usage.thumbnails_pending > 0 && (
        <p className="text-xs text-ink-3">
          {usage.thumbnails_pending} awaiting a server-side thumbnail
        </p>
      )}
    </section>
  );
}
