import { createContext, useContext } from "react";

import {
  clearSession,
  getCSRFToken,
  setAuthenticated,
  setCSRFToken,
} from "../auth/session";
import { ContractError } from "./error";
import { apiMetaPath, apiResourcePath } from "./paths";
import type { Catalog, CatalogAux, ModelMeta, PageMeta, Row } from "./types";

export type Envelope<T, M = unknown> = {
  data: T;
  meta?: M;
};

const DEFAULT_API_PREFIX = "/api/v1";
const PREFIX_PLACEHOLDER = "__GOMBIT_API_PREFIX__";

function apiOrigin(): string {
  return import.meta.env.VITE_API_URL ?? "";
}

/**
 * Runtime API prefix (ADMIN-1 honors config.API.Prefix). Read from the
 * served index (`window.__GOMBIT_API_PREFIX__` or
 * `<meta name="gombit-api-prefix">`), not from VITE_* (that would freeze
 * the prefix in committed dist). Default remains `/api/v1`.
 */
export function apiPrefix(): string {
  const injected = readInjectedAPIPrefix();
  const raw = (injected ?? "").trim();
  if (raw === "" || raw === PREFIX_PLACEHOLDER) {
    return DEFAULT_API_PREFIX;
  }
  return raw.replace(/\/+$/, "") || DEFAULT_API_PREFIX;
}

function readInjectedAPIPrefix(): string | undefined {
  if (typeof window !== "undefined" && typeof window.__GOMBIT_API_PREFIX__ === "string") {
    return window.__GOMBIT_API_PREFIX__;
  }
  if (typeof document !== "undefined") {
    const content = document.querySelector('meta[name="gombit-api-prefix"]')?.getAttribute("content");
    if (content) {
      return content;
    }
  }
  return undefined;
}

function apiPath(path: string): string {
  const suffix = path.startsWith("/") ? path : `/${path}`;
  return apiPrefix() + suffix;
}

/**
 * Thin same-origin fetch wrapper around D10 `{data, meta, error}`.
 * Cookie session + CSRF double-submit (X-CSRF-Token) on unsafe methods. The
 * token is read from the JS-readable gombit_csrf cookie at request time.
 * Concurrent 401s share one refreshInFlight promise; a 401 silent-refreshes and
 * a 403 re-bootstraps CSRF, each retrying the request once.
 */
export function createAdminClient() {
  const baseUrl = apiOrigin();
  let refreshInFlight: Promise<boolean> | null = null;

  async function refreshSession(): Promise<boolean> {
    if (refreshInFlight) {
      return refreshInFlight;
    }
    refreshInFlight = (async () => {
      try {
        // POST /auth/refresh is CSRF-protected. AppProviders fire-and-forget
        // bootstrapCSRF; a 401 on GET /me can race that GET /auth/csrf.
        await bootstrapCSRF();
        const response = await fetch(baseUrl + apiPath("/auth/refresh"), {
          method: "POST",
          credentials: "same-origin",
          headers: csrfRequestHeaders(),
        });
        if (!response.ok) {
          throw new Error(`refresh failed: ${response.status}`);
        }
        setAuthenticated(true);
        return true;
      } catch {
        clearSession();
        return false;
      } finally {
        refreshInFlight = null;
      }
    })();
    return refreshInFlight;
  }

  async function request<T, M = unknown>(
    method: string,
    path: string,
    options?: { body?: unknown; query?: Record<string, string | number | undefined> },
  ): Promise<Envelope<T, M>> {
    const url = buildURL(baseUrl, path, options?.query);
    const init = (): RequestInit => {
      const headers = new Headers();
      if (options?.body !== undefined) {
        headers.set("Content-Type", "application/json");
      }
      if (isUnsafeMethod(method)) {
        // Read the token at request time from the JS-readable gombit_csrf
        // cookie (the authoritative double-submit value), so a cookie rotated
        // or re-minted by another tab / after expiry is always matched (#250).
        const token = currentCSRFToken();
        if (token) {
          headers.set("X-CSRF-Token", token);
        }
      }
      return {
        method,
        credentials: "same-origin",
        headers,
        body: options?.body === undefined ? undefined : JSON.stringify(options.body),
      };
    };

    if (isUnsafeMethod(method)) {
      await bootstrapCSRF();
    }

    let response = await fetch(url, init());
    if (response.status === 401 && !isAuthPath(path)) {
      const ok = await refreshSession();
      if (ok) {
        response = await fetch(url, init());
      }
    } else if (response.status === 403 && isUnsafeMethod(method)) {
      // A CSRF 403 means our cookie was rotated/expired out from under us.
      // Drop the stale in-memory mirror, force a fresh bootstrap, and retry
      // once — mirroring the 401 silent-refresh path (#250).
      setCSRFToken(undefined);
      await bootstrapCSRF(true);
      response = await fetch(url, init());
    }

    const parsed: unknown = await readJSON(response);
    if (!response.ok) {
      throw ContractError.fromResponse(response, parsed);
    }
    if (parsed === null || typeof parsed !== "object" || !("data" in parsed)) {
      throw new ContractError("empty_response", "Empty response", response.status);
    }
    const envelope = parsed as Envelope<T, M>;
    return envelope;
  }

  return {
    get: <T, M = unknown>(path: string, query?: Record<string, string | number | undefined>) =>
      request<T, M>("GET", path, { query }),
    post: <T, M = unknown>(path: string, body?: unknown) => request<T, M>("POST", path, { body }),
    patch: <T, M = unknown>(path: string, body?: unknown) => request<T, M>("PATCH", path, { body }),
    delete: <T, M = unknown>(path: string) => request<T, M>("DELETE", path),
    login: (email: string, password: string) =>
      request<{ id: number; email: string }>("POST", apiPath("/auth/login"), {
        body: { email, password },
      }),
    logout: () => request<{ ok: boolean }>("POST", apiPath("/auth/logout")),
    me: () => request<{ id: number; email: string }>("GET", apiPath("/me")),
    catalog: () => request<Catalog, CatalogAux>("GET", apiPath("/admin/meta")),
    model: (slug: string) => request<ModelMeta>("GET", apiPath(apiMetaPath(slug))),
    list: (slug: string, query?: Record<string, string | number | undefined>) =>
      request<Row[], PageMeta>("GET", apiPath(apiResourcePath(slug)), { query }),
    create: (slug: string, body: Row) =>
      request<Row>("POST", apiPath(apiResourcePath(slug)), { body }),
    detail: (slug: string, id: string) =>
      request<Row>("GET", apiPath(apiResourcePath(slug, id))),
    update: (slug: string, id: string, body: Row) =>
      request<Row>("PATCH", apiPath(apiResourcePath(slug, id)), { body }),
    remove: (slug: string, id: string) =>
      request<{ ok: boolean }>("DELETE", apiPath(apiResourcePath(slug, id))),
  };
}

export type AdminClient = ReturnType<typeof createAdminClient>;

export const ApiClientContext = createContext<AdminClient | null>(null);

export function useApiClient(): AdminClient {
  const client = useContext(ApiClientContext);
  if (client === null) {
    throw new Error("useApiClient must be used within AppProviders");
  }
  return client;
}

let csrfInFlight: Promise<void> | null = null;

/**
 * Ensures a CSRF cookie exists and mirrors its token in memory. Keys off the
 * JS-readable gombit_csrf cookie, not the in-memory token: if the cookie is
 * already present this is a no-op (a second tab's bootstrap must not matter),
 * so concurrent tabs converge on the one shared cookie. GET /auth/csrf reuses a
 * valid cookie server-side (#250), so overlapping bootstraps no longer rotate
 * it. Pass force=true to re-fetch even when a cookie is present (403 recovery).
 * Concurrent callers share one in-flight promise. Unsafe requests and silent
 * refresh await this before POST so a 401 cannot race GET /auth/csrf.
 */
export function bootstrapCSRF(force = false): Promise<void> {
  if (!force && readCSRFCookie()) {
    return Promise.resolve();
  }
  if (csrfInFlight) {
    return csrfInFlight;
  }
  csrfInFlight = (async () => {
    try {
      const response = await fetch(apiOrigin() + apiPath("/auth/csrf"), {
        credentials: "same-origin",
      });
      if (!response.ok) {
        return;
      }
      const body = (await response.json()) as { data?: { csrf_token?: string } };
      if (body.data?.csrf_token) {
        setCSRFToken(body.data.csrf_token);
      }
    } finally {
      csrfInFlight = null;
    }
  })();
  return csrfInFlight;
}

/**
 * Reads the JS-readable gombit_csrf cookie — the authoritative double-submit
 * value. The cookie is deliberately not HttpOnly so the SPA can echo it.
 */
function readCSRFCookie(): string {
  if (typeof document === "undefined") {
    return "";
  }
  const match = document.cookie.match(/(?:^|;\s*)gombit_csrf=([^;]*)/);
  return match ? decodeURIComponent(match[1]) : "";
}

/** Current CSRF token: the live cookie wins over the in-memory mirror. */
function currentCSRFToken(): string {
  return readCSRFCookie() || getCSRFToken() || "";
}

function csrfRequestHeaders(): HeadersInit {
  const token = currentCSRFToken();
  return token ? { "X-CSRF-Token": token } : {};
}

function isUnsafeMethod(method: string): boolean {
  return method !== "GET" && method !== "HEAD" && method !== "OPTIONS";
}

function isAuthPath(path: string): boolean {
  return (
    path.endsWith("/auth/login") ||
    path.endsWith("/auth/refresh") ||
    path.endsWith("/auth/logout") ||
    path.endsWith("/auth/register") ||
    path.endsWith("/auth/csrf")
  );
}

function buildURL(
  baseUrl: string,
  path: string,
  query?: Record<string, string | number | undefined>,
): string {
  const origin = typeof window !== "undefined" ? window.location.origin : "http://127.0.0.1";
  const url = new URL(baseUrl + path, origin);
  if (query) {
    for (const [key, value] of Object.entries(query)) {
      if (value === undefined || value === "") {
        continue;
      }
      url.searchParams.set(key, String(value));
    }
  }
  return url.toString();
}

async function readJSON(response: Response): Promise<unknown> {
  const text = await response.text();
  if (text === "") {
    return undefined;
  }
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return undefined;
  }
}
