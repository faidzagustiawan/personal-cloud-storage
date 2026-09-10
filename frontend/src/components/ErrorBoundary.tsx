import { Component, type ErrorInfo, type ReactNode } from "react";

interface State {
  error: Error | null;
}

/**
 * Last resort for a render-time crash.
 *
 * Without one, a single bad component unmounts the whole tree and the user gets
 * a white page with no explanation and no way back. This at least names the
 * failure and offers a reload.
 */
export class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error("render failed", error, info.componentStack);
  }

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;

    return (
      <main className="flex min-h-full items-center justify-center p-6">
        <div className="flex max-w-md flex-col gap-3">
          <h1 className="text-lg font-semibold text-ink">This page stopped working</h1>
          <p className="text-sm text-ink-2">
            Nothing was lost — your photos are stored separately from this app. Reloading usually
            clears it.
          </p>
          <pre className="tabular overflow-x-auto rounded-sm border-l-2 border-l-danger bg-danger-soft px-3 py-2 text-xs text-danger">
            {error.message}
          </pre>
          <button
            type="button"
            onClick={() => window.location.reload()}
            className="self-start rounded-sm bg-accent px-4 py-2 text-sm font-semibold text-accent-ink"
          >
            Reload
          </button>
        </div>
      </main>
    );
  }
}
