import { Link, Outlet, useNavigate } from "react-router";

import { AppBar, Box, Button, Toolbar, Typography } from "@mui/material";

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
    <Box>
      <AppBar position="static">
        <Toolbar>
          <Typography variant="h6" component="div" sx={{ flexGrow: 1 }}>
            demo
          </Typography>
          <Button color="inherit" component={Link} to="/">
            Products
          </Button>
          <Button color="inherit" component={Link} to="/products/new">
            New product
          </Button>
          {generatedResources.map((resource) => (
            <Button key={resource.slug} color="inherit" component={Link} to={resource.listPath}>
              {resource.title}
            </Button>
          ))}
          <Button color="inherit" component={Link} to="/sessions">
            Sessions
          </Button>
          <Button color="inherit" onClick={() => void onLogout()}>
            Log out
          </Button>
        </Toolbar>
      </AppBar>
      <Box component="main" sx={{ p: 3 }}>
        <Outlet />
      </Box>
    </Box>
  );
}
