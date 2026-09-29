import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const backend = process.env.GOMBIT_DEV_BACKEND ?? "http://127.0.0.1:8080";
const host = process.env.GOMBIT_DEV_FRONTEND_HOST ?? "127.0.0.1";
const port = Number(process.env.GOMBIT_DEV_FRONTEND_PORT ?? "5173");
const apiPrefix = normalizeAPIPrefix(process.env.GOMBIT_API_PREFIX);

function normalizeAPIPrefix(raw: string | undefined): string {
  const prefix = (raw ?? "").trim().replace(/\/+$/, "");
  return prefix === "" ? "/api/v1" : prefix;
}

function escapeHTML(value: string): string {
  return value
    .replace(/&/g, "&amp;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

/**
 * Replace __GOMBIT_API_PREFIX__ during `vite dev` so `gombit dev` honors
 * the live GOMBIT_API_PREFIX. Production builds leave the placeholder so
 * `gombit build --embed` can inject config.API.Prefix when Gin serves
 * index.html (same model as the admin SPA).
 */
function injectAPIPrefix() {
  const placeholder = "__GOMBIT_API_PREFIX__";
  return {
    name: "gombit-api-prefix",
    transformIndexHtml: {
      order: "pre" as const,
      handler(html: string, ctx: { server?: unknown }) {
        if (!ctx.server) {
          return html;
        }
        return html.replaceAll(placeholder, escapeHTML(apiPrefix));
      },
    },
  };
}

const proxy: Record<string, { target: string; changeOrigin: boolean }> = {
  "/api": { target: backend, changeOrigin: true },
  "/openapi.json": { target: backend, changeOrigin: true },
  "/docs": { target: backend, changeOrigin: true },
  "/admin": { target: backend, changeOrigin: true },
  // The app serves stored files and their direct uploads here
  // (GOMBIT_STORAGE_LOCAL_URL) with the local and memory drivers.
  "/_storage": { target: backend, changeOrigin: true },
};
if (apiPrefix !== "/" && apiPrefix !== "/api" && !apiPrefix.startsWith("/api/")) {
  proxy[apiPrefix] = { target: backend, changeOrigin: true };
}

export default defineConfig({
  plugins: [react(), injectAPIPrefix()],
  server: {
    host,
    port,
    strictPort: true,
    proxy,
  },
});
