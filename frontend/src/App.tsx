import { Navigate, Outlet, Route, Routes, useLocation } from "react-router-dom";

import { AppLayout } from "./components/AppLayout";
import { Notice, Spinner } from "./components/ui";
import { useMe } from "./hooks/queries";
import { GalleryPage } from "./pages/GalleryPage";
import { LoginPage } from "./pages/LoginPage";
import { SettingsPage } from "./pages/SettingsPage";

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />

      <Route element={<RequireAuth />}>
        <Route element={<AppLayout />}>
          <Route index element={<GalleryPage />} />
          <Route path="folder/:folderId" element={<GalleryPage />} />
          <Route path="trash" element={<GalleryPage trash />} />
          <Route path="settings" element={<SettingsPage />} />
        </Route>
      </Route>

      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}

/**
 * Gates every route behind a session.
 *
 * The check is a single query for the current user, so an expired cookie
 * produces one redirect rather than a wall of failed requests. The path being
 * visited is carried along so signing in lands where the user was going.
 */
function RequireAuth() {
  const { data: me, isPending, error } = useMe();
  const location = useLocation();

  if (isPending) {
    return (
      <div className="flex min-h-full items-center justify-center">
        <Spinner className="text-ink-3" />
      </div>
    );
  }

  if (error) {
    return (
      <div className="mx-auto max-w-md p-6">
        <Notice>{error.message}</Notice>
      </div>
    );
  }

  if (!me) {
    return <Navigate to="/login" replace state={{ from: location.pathname }} />;
  }

  return <Outlet />;
}
