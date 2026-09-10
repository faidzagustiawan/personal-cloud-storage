import type { ButtonHTMLAttributes, InputHTMLAttributes, ReactNode } from "react";

type ButtonVariant = "primary" | "ghost" | "danger";

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  loading?: boolean;
}

const variants: Record<ButtonVariant, string> = {
  primary: "bg-accent text-accent-ink hover:brightness-110",
  ghost: "border border-line text-ink-2 hover:bg-surface-2 hover:text-ink",
  danger: "bg-danger-soft text-danger hover:brightness-105",
};

export function Button({
  variant = "primary",
  loading = false,
  className = "",
  disabled,
  children,
  ...rest
}: ButtonProps) {
  return (
    <button
      {...rest}
      disabled={disabled || loading}
      className={`inline-flex items-center justify-center gap-2 rounded-sm px-4 py-2 text-sm font-semibold transition disabled:cursor-not-allowed disabled:opacity-55 ${variants[variant]} ${className}`}
    >
      {loading && <Spinner />}
      {children}
    </button>
  );
}

export function Spinner({ className = "" }: { className?: string }) {
  return (
    <span
      role="status"
      aria-label="Loading"
      className={`inline-block h-3.5 w-3.5 animate-spin rounded-full border-2 border-current border-t-transparent ${className}`}
    />
  );
}

export function Field({
  label,
  hint,
  ...rest
}: InputHTMLAttributes<HTMLInputElement> & { label: string; hint?: string }) {
  return (
    <label className="flex flex-col gap-1.5">
      <span className="text-xs font-semibold tracking-wide text-ink-2 uppercase">{label}</span>
      <input
        {...rest}
        className="rounded-sm border border-line bg-surface px-3 py-2 text-sm text-ink placeholder:text-ink-3"
      />
      {hint && <span className="text-xs text-ink-3">{hint}</span>}
    </label>
  );
}

/**
 * Errors say what went wrong and, where the user can act, what to do about it.
 * No apologies, no "oops".
 */
export function Notice({
  tone = "error",
  children,
}: {
  tone?: "error" | "info";
  children: ReactNode;
}) {
  const skin =
    tone === "error"
      ? "border-l-danger bg-danger-soft text-danger"
      : "border-l-accent bg-accent-soft text-ink";
  return (
    <div role="alert" className={`rounded-sm border-l-2 px-3 py-2 text-sm ${skin}`}>
      {children}
    </div>
  );
}

export function EmptyState({
  title,
  body,
  action,
}: {
  title: string;
  body: string;
  action?: ReactNode;
}) {
  return (
    <div className="flex flex-col items-center gap-3 px-6 py-16 text-center">
      <h2 className="text-base font-semibold text-ink">{title}</h2>
      <p className="max-w-sm text-sm text-ink-2">{body}</p>
      {action}
    </div>
  );
}
