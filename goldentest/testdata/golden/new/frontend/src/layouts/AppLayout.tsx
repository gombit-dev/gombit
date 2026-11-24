import { Link, Outlet, useNavigate } from "react-router";

import { useApiClient } from "../api/client";
import { clearSession, getRefreshToken } from "../auth/session";
import { generatedResources } from "../resources";

export function AppLayout() {
  const client = useApiClient();
  const navigate = useNavigate();

  async function onLogout() {
    try {
      const refreshToken = getRefreshToken();
      if (refreshToken) {
        await client.POST("/api/v1/auth/logout", { body: { refresh_token: refreshToken } });
      }
      } finally {
      clearSession();
      navigate("/login", { replace: true });
    }
  }

  return (
    <div>
      <header>
        <nav>
          <Link to="/">Products</Link>
          {" · "}
          <Link to="/products/new">New product</Link>
          {generatedResources.map((resource) => (
            <span key={resource.slug}>
              {" · "}
              <Link to={resource.listPath}>{resource.title}</Link>
            </span>
          ))}
          {" · "}
          <Link to="/sessions">Sessions</Link>
          {" · "}
          <button type="button" onClick={() => void onLogout()}>
            Log out
          </button>
        </nav>
      </header>
      <main>
        <Outlet />
      </main>
    </div>
  );
}
