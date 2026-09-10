import { type FormEvent, useState } from "react";
import { Navigate, useLocation } from "react-router-dom";

import { Button, Field, Notice } from "../components/ui";
import { useLogin, useMe } from "../hooks/queries";
import { ApiError } from "../lib/api";

export function LoginPage() {
  const { data: me, isPending } = useMe();
  const login = useLogin();
  const location = useLocation();

  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");

  if (isPending) return null;
  if (me) {
    const from = (location.state as { from?: string } | null)?.from;
    return <Navigate to={from ?? "/"} replace />;
  }

  function onSubmit(event: FormEvent) {
    event.preventDefault();
    login.mutate({ username, password });
  }

  const error = login.error;
  const message =
    error instanceof ApiError
      ? error.message
      : error
        ? "Cannot reach the server. Check your connection."
        : null;

  return (
    <main className="flex min-h-full items-center justify-center px-5 py-16">
      <div className="flex w-full max-w-sm flex-col gap-6">
        <header className="flex flex-col gap-1">
          <span className="tabular text-[11px] tracking-[0.16em] text-ink-3 uppercase">
            cloud.faidz.fun
          </span>
          <h1 className="text-2xl font-bold tracking-tight text-ink">Sign in</h1>
        </header>

        <form onSubmit={onSubmit} className="flex flex-col gap-4">
          <Field
            label="Username"
            name="username"
            autoComplete="username"
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
            required
            value={username}
            onChange={(e) => setUsername(e.target.value)}
          />
          <Field
            label="Password"
            name="password"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />

          {message && <Notice>{message}</Notice>}

          <Button type="submit" loading={login.isPending}>
            Sign in
          </Button>
        </form>

        {/* There is no registration endpoint at all (decision D7), so the page
            does not offer one. Saying where accounts come from is more useful
            than a dead "sign up" link. */}
        <p className="text-xs leading-relaxed text-ink-3">
          Accounts are created on the server with{" "}
          <code className="tabular text-ink-2">cloudapp createuser</code>. There is no public
          sign-up.
        </p>
      </div>
    </main>
  );
}
