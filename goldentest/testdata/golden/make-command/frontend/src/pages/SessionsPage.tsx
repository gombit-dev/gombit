import { useEffect, useState } from "react";
import { useNavigate } from "react-router";

import { useApiClient } from "../api/client";
import { ContractError, unwrap } from "../api/generated/client";
import type { components } from "../api/generated/schema";
import { clearSession } from "../auth/session";

type Session = components["schemas"]["AuthSession"];

/**
 * The signed-in user's sessions (GET {prefix}/auth/sessions): one per
 * sign-in, the current one marked, each signed out on its own, or all the
 * others, or all of them. Ending the current session, or all of them,
 * signs this browser out. The page is the app's own: change it freely.
 */
export function SessionsPage() {
  const client = useApiClient();
  const navigate = useNavigate();
  const [sessions, setSessions] = useState<Session[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  // Bumped to list the sessions again.
  const [version, setVersion] = useState(0);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const listed = await unwrap(await client.GET("/api/v1/auth/sessions"));
        if (!cancelled) {
          setSessions(listed && Array.isArray(listed.data) ? listed.data : []);
        }
      } catch (err: unknown) {
        if (!cancelled) {
          failed(err);
        }
      } finally {
        if (!cancelled) {
          setLoading(false);
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [client, version]);

  function signedOut() {
    clearSession();
    navigate("/login", { replace: true });
  }

  function failed(err: unknown) {
    if (err instanceof ContractError && err.status === 401) {
      // This browser's own session has ended (signed out elsewhere, or
      // expired), and the client's refresh could not renew it.
      signedOut();
      return;
    }
    if (err instanceof ContractError && err.status === 404) {
      // The session had already ended (signed out on its device, or by
      // another tab): list what is left.
      setStatus("That session had already ended.");
      setVersion((v) => v + 1);
      return;
    }
    setStatus(err instanceof Error ? err.message : "request failed");
  }

  async function revoke(session: Session) {
    setBusy(true);
    setStatus("");
    try {
      await unwrap(
        await client.DELETE("/api/v1/auth/sessions/{id}", {
          params: { path: { id: session.id } },
        }),
      );
      if (session.current) {
        signedOut();
        return;
      }
      setSessions((listed) => listed.filter((s) => s.id !== session.id));
    } catch (err: unknown) {
      failed(err);
    } finally {
      setBusy(false);
    }
  }

  async function revokeAll(scope: "others" | "all") {
    setBusy(true);
    setStatus("");
    try {
      await unwrap(
        await client.DELETE("/api/v1/auth/sessions", {
          params: { query: { scope } },
        }),
      );
      if (scope === "all") {
        signedOut();
        return;
      }
      setSessions((listed) => listed.filter((s) => s.current));
    } catch (err: unknown) {
      failed(err);
    } finally {
      setBusy(false);
    }
  }

  const others = sessions.some((s) => !s.current);

  return (
    <section>
      <h1>Sessions</h1>
      <p>Each sign-in is a session. Sign out the ones you do not recognize.</p>
      <p>
        <button type="button" disabled={busy || !others} onClick={() => void revokeAll("others")}>
          Sign out other sessions
        </button>{" "}
        <button type="button" disabled={busy} onClick={() => void revokeAll("all")}>
          Sign out everywhere
        </button>
      </p>
      {status ? <p>{status}</p> : null}
      {loading ? (
        <p>Loading sessions…</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Device</th>
              <th>Signed in from</th>
              <th>Last active</th>
              <th>Expires</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {sessions.map((session) => (
              <tr key={session.id}>
                <td title={session.user_agent}>
                  {describeDevice(session.user_agent)}
                  {session.current ? <strong> (this device)</strong> : null}
                </td>
                <td>{describePlace(session)}</td>
                <td>{formatTime(session.last_refreshed_at)}</td>
                <td>{formatTime(session.expires_at)}</td>
                <td>
                  <button type="button" disabled={busy} onClick={() => void revoke(session)}>
                    Sign out
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

/**
 * A short label for a User-Agent ("Firefox on Linux"), from a few common
 * patterns; the raw value otherwise. The server stores the header as the
 * browser sent it and leaves the label to the app: swap in a full parser
 * if you need one.
 */
function describeDevice(userAgent: string): string {
  if (userAgent === "") {
    return "Unknown device";
  }
  const browser = firstMatch(userAgent, [
    [/Edg\//, "Edge"],
    [/OPR\//, "Opera"],
    [/Firefox\//, "Firefox"],
    [/Chrome\//, "Chrome"],
    [/Safari\//, "Safari"],
  ]);
  const os = firstMatch(userAgent, [
    [/iPhone|iPad/, "iOS"],
    [/Android/, "Android"],
    [/Windows/, "Windows"],
    [/Mac OS X|Macintosh/, "macOS"],
    [/CrOS/, "ChromeOS"],
    [/Linux/, "Linux"],
  ]);
  if (browser && os) {
    return `${browser} on ${os}`;
  }
  return browser || os || userAgent;
}

function firstMatch(value: string, patterns: [RegExp, string][]): string {
  for (const [pattern, name] of patterns) {
    if (pattern.test(value)) {
      return name;
    }
  }
  return "";
}

/** Where a session signed in from: its location and network, as known. */
function describePlace(session: Session): string {
  const parts = [session.location, session.client_address].filter((part) => part !== "");
  return parts.length > 0 ? parts.join(" · ") : "Unknown";
}

function formatTime(value: string): string {
  const time = new Date(value);
  return Number.isNaN(time.getTime()) ? value : time.toLocaleString();
}
