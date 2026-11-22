import { useEffect, useState, type CSSProperties } from "react";
import { useNavigate } from "react-router";

import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from "@mui/material";

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
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  // version is bumped to list the sessions again; listed is the version
  // the table shows. While they differ a list is in flight and the buttons
  // wait for it: a sign-out that answered before an older list would be
  // undone when that list lands.
  const [version, setVersion] = useState(0);
  const [listed, setListed] = useState(-1);
  const listing = listed !== version;
  const loading = listed < 0;

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const page = await unwrap(await client.GET("/api/v1/auth/sessions"));
        if (!cancelled) {
          setSessions(page && Array.isArray(page.data) ? page.data : []);
        }
      } catch (err: unknown) {
        // Never list again from here: the list never answers 404 for an
        // ended session, so a 404 (or any other failure) would only repeat.
        if (!cancelled && !sessionEnded(err)) {
          setStatus(message(err));
        }
      } finally {
        if (!cancelled) {
          setListed(version);
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

  // sessionEnded signs this browser out when a request answered 401: its
  // own session has ended (signed out elsewhere, or expired), and the
  // client's refresh could not renew it.
  function sessionEnded(err: unknown): boolean {
    if (err instanceof ContractError && err.status === 401) {
      signedOut();
      return true;
    }
    return false;
  }

  // revokeFailed handles a failed sign-out of one session. Only there does
  // 404 mean the session had already ended (signed out on its device, or by
  // another tab), so only there does the page list what is left.
  function revokeFailed(err: unknown) {
    if (sessionEnded(err)) {
      return;
    }
    if (err instanceof ContractError && err.status === 404) {
      setStatus("That session had already ended.");
      setVersion((v) => v + 1);
      return;
    }
    setStatus(message(err));
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
      setSessions((rows) => rows.filter((s) => s.id !== session.id));
    } catch (err: unknown) {
      revokeFailed(err);
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
      setSessions((rows) => rows.filter((s) => s.current));
    } catch (err: unknown) {
      if (!sessionEnded(err)) {
        setStatus(message(err));
      }
    } finally {
      setBusy(false);
    }
  }

  const others = sessions.some((s) => !s.current);
  const waiting = busy || listing;

  return (
    <Box>
      <Typography variant="h4" component="h1" sx={{ mb: 2 }}>
        Sessions
      </Typography>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
        Each sign-in is a session. Sign out the ones you do not recognize.
      </Typography>
      <Stack direction="row" spacing={2} sx={{ mb: 2 }}>
        <Button variant="outlined" disabled={waiting || !others} onClick={() => void revokeAll("others")}>
          Sign out other sessions
        </Button>
        <Button variant="outlined" color="error" disabled={waiting} onClick={() => void revokeAll("all")}>
          Sign out everywhere
        </Button>
      </Stack>
      {status ? (
        <Alert severity="info" sx={{ mb: 2 }}>
          {status}
        </Alert>
      ) : null}
      {loading ? (
        <Box sx={{ display: "flex", justifyContent: "center", py: 6 }}>
          <CircularProgress />
        </Box>
      ) : (
        <TableContainer component={Paper}>
          <Table>
            <TableHead>
              <TableRow>
                <TableCell>Device</TableCell>
                <TableCell>Signed in from</TableCell>
                <TableCell>Last active</TableCell>
                <TableCell>Expires</TableCell>
                <TableCell>
                  <span style={visuallyHidden}>Actions</span>
                </TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {sessions.map((session) => (
                <TableRow key={session.id}>
                  <TableCell title={session.user_agent}>
                    {describeDevice(session.user_agent)}{" "}
                    {session.current ? <Chip size="small" color="primary" label="This device" /> : null}
                  </TableCell>
                  <TableCell>{describePlace(session)}</TableCell>
                  <TableCell>{formatTime(session.last_refreshed_at)}</TableCell>
                  <TableCell>{formatTime(session.expires_at)}</TableCell>
                  <TableCell align="right">
                    <Button
                      size="small"
                      disabled={waiting}
                      aria-label={`Sign out ${sessionName(session)}`}
                      onClick={() => void revoke(session)}
                    >
                      Sign out
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableContainer>
      )}
    </Box>
  );
}

/**
 * A short label for a User-Agent ("Firefox on Linux"), from a few common
 * patterns; the raw value otherwise. The server stores the header as the
 * browser sent it and leaves the label to the app: swap in a full parser
 * if you need one. A User-Agent is only a hint: iPadOS sends a Mac one by
 * default (so an iPad reads "Safari on macOS"), and any browser can send
 * whatever it likes.
 */
function describeDevice(userAgent: string): string {
  if (userAgent === "") {
    return "Unknown device";
  }
  // Order matters: most browsers also name the engines they are built on
  // (Edge and Opera say Chrome, and every iOS browser says Safari).
  const browser = firstMatch(userAgent, [
    [/Edg(e|A|iOS)?\//, "Edge"],
    [/OPR\/|OPT\//, "Opera"],
    [/SamsungBrowser\//, "Samsung Internet"],
    [/Firefox\/|FxiOS\//, "Firefox"],
    [/Chrome\/|CriOS\//, "Chrome"],
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

/** A row's name for assistive tech: which session its "Sign out" ends. */
function sessionName(session: Session): string {
  const name = `${describeDevice(session.user_agent)}, ${describePlace(session)}, last active ${formatTime(session.last_refreshed_at)}`;
  return session.current ? `${name} (this device)` : name;
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : "request failed";
}

/** Hides a label from the eye but not from screen readers. */
const visuallyHidden: CSSProperties = {
  position: "absolute",
  width: 1,
  height: 1,
  padding: 0,
  margin: -1,
  overflow: "hidden",
  clip: "rect(0 0 0 0)",
  whiteSpace: "nowrap",
  border: 0,
};
