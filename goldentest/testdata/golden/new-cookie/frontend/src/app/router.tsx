import { BrowserRouter, Navigate, Route, Routes } from "react-router";

import { RequireAuth } from "../auth/RequireAuth";
import { AppLayout } from "../layouts/AppLayout";
import { LoginPage } from "../pages/LoginPage";
import { ProductFormPage } from "../pages/ProductFormPage";
import { ProductListPage } from "../pages/ProductListPage";
import { SessionsPage } from "../pages/SessionsPage";
import { generatedResourceRoutes } from "../resources";

export function AppRouter() {
  return (
    <BrowserRouter>
      <Routes>
        <Route path="login" element={<LoginPage />} />
        <Route element={<RequireAuth />}>
          <Route element={<AppLayout />}>
            <Route index element={<ProductListPage />} />
            <Route path="products/new" element={<ProductFormPage />} />
            <Route path="sessions" element={<SessionsPage />} />
            {generatedResourceRoutes.map((route) => (
              <Route key={String(route.path)} path={route.path} element={route.element} />
            ))}
            <Route path="*" element={<Navigate to="/" replace />} />
          </Route>
        </Route>
      </Routes>
    </BrowserRouter>
  );
}
