import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";

import { App } from "./App";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { ApiError } from "./lib/api";
import { applyTheme, readTheme } from "./lib/theme";
import "./index.css";

applyTheme(readTheme());

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Retrying a 401 or a 404 just delays the answer. Only genuinely
      // transient failures — a storage backend briefly unavailable — are worth
      // a second attempt.
      retry: (failureCount, error) => {
        if (error instanceof ApiError) return error.isTransient && failureCount < 2;
        return failureCount < 1;
      },
      refetchOnWindowFocus: false,
      staleTime: 15_000,
    },
    mutations: { retry: false },
  },
});

const container = document.getElementById("root");
if (!container) throw new Error("#root is missing from index.html");

createRoot(container).render(
  <StrictMode>
    <ErrorBoundary>
      <QueryClientProvider client={queryClient}>
        <BrowserRouter>
          <App />
        </BrowserRouter>
      </QueryClientProvider>
    </ErrorBoundary>
  </StrictMode>,
);
