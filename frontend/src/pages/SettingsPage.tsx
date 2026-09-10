import { type FormEvent, useState } from "react";

import { SessionList } from "../components/SessionList";
import { ShareList } from "../components/ShareList";
import { Button, Field, Notice } from "../components/ui";
import { useChangePassword, useMe, useUsage } from "../hooks/queries";
import { formatBytes, formatDateTime } from "../lib/format";
import { applyTheme, readTheme, type Theme } from "../lib/theme";

export function SettingsPage() {
  const { data: me } = useMe();
  const usage = useUsage();
  const [theme, setTheme] = useState<Theme>(readTheme);

  return (
    <div className="flex max-w-2xl flex-col gap-8 p-5">
      <section className="flex flex-col gap-3">
        <SectionTitle>Account</SectionTitle>
        <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5 text-sm">
          <dt className="text-ink-2">Signed in as</dt>
          <dd className="text-ink">{me?.username}</dd>
          {me?.last_login_at && (
            <>
              <dt className="text-ink-2">Last sign-in</dt>
              <dd className="tabular text-ink">{formatDateTime(me.last_login_at)}</dd>
            </>
          )}
        </dl>
      </section>

      <section className="flex flex-col gap-3">
        <SectionTitle>Storage</SectionTitle>
        {usage.data ? (
          <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5 text-sm">
            <dt className="text-ink-2">Used</dt>
            <dd className="tabular text-ink">
              {formatBytes(usage.data.used_bytes)} of {formatBytes(usage.data.quota_bytes)}
            </dd>
            <dt className="text-ink-2">Photos</dt>
            <dd className="tabular text-ink">{formatBytes(usage.data.image_bytes)}</dd>
            <dt className="text-ink-2">Videos</dt>
            <dd className="tabular text-ink">{formatBytes(usage.data.video_bytes)}</dd>
            <dt className="text-ink-2">In trash</dt>
            <dd className="tabular text-ink">
              {formatBytes(usage.data.trash_bytes)}
              {usage.data.trash_bytes > 0 && (
                <span className="ml-2 font-sans text-xs text-ink-3">
                  released 30 days after deletion
                </span>
              )}
            </dd>
            <dt className="text-ink-2">Files</dt>
            <dd className="tabular text-ink">{usage.data.file_count}</dd>
          </dl>
        ) : (
          <p className="text-sm text-ink-3">Loading…</p>
        )}
      </section>

      <section className="flex flex-col gap-3">
        <SectionTitle>Appearance</SectionTitle>
        <div className="flex gap-1.5">
          {(["system", "light", "dark"] as const).map((option) => (
            <button
              key={option}
              type="button"
              onClick={() => {
                setTheme(option);
                applyTheme(option);
              }}
              className={`rounded-sm border px-3 py-1.5 text-sm capitalize transition ${
                theme === option
                  ? "border-accent bg-accent-soft font-semibold text-ink"
                  : "border-line text-ink-2 hover:bg-surface-2"
              }`}
            >
              {option}
            </button>
          ))}
        </div>
      </section>

      <section className="flex flex-col gap-3">
        <SectionTitle>Shared links</SectionTitle>
        <ShareList />
      </section>

      <section className="flex flex-col gap-3">
        <SectionTitle>Signed-in devices</SectionTitle>
        <SessionList />
      </section>

      <ChangePasswordForm />
    </div>
  );
}

function ChangePasswordForm() {
  const change = useChangePassword();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");

  function onSubmit(event: FormEvent) {
    event.preventDefault();
    change.mutate(
      { current, next },
      {
        onSuccess: () => {
          setCurrent("");
          setNext("");
        },
      },
    );
  }

  return (
    <section className="flex flex-col gap-3">
      <SectionTitle>Change password</SectionTitle>
      <form onSubmit={onSubmit} className="flex max-w-xs flex-col gap-3">
        <Field
          label="Current password"
          type="password"
          autoComplete="current-password"
          required
          value={current}
          onChange={(e) => setCurrent(e.target.value)}
        />
        <Field
          label="New password"
          type="password"
          autoComplete="new-password"
          required
          minLength={10}
          hint="At least 10 characters."
          value={next}
          onChange={(e) => setNext(e.target.value)}
        />

        {change.error && <Notice>{change.error.message}</Notice>}
        {change.isSuccess && (
          <Notice tone="info">
            Password changed. Every other signed-in session was signed out.
          </Notice>
        )}

        <Button type="submit" loading={change.isPending}>
          Change password
        </Button>
      </form>
    </section>
  );
}

function SectionTitle({ children }: { children: string }) {
  return (
    <h2 className="border-b border-line pb-1.5 text-[11px] font-semibold tracking-[0.12em] text-ink-2 uppercase">
      {children}
    </h2>
  );
}
