import { useEffect, useState } from "react";
import { Link, Navigate, useNavigate, useParams } from "react-router";

import {
  Alert,
  Box,
  Button,
  CircularProgress,
  Paper,
  Table,
  TableBody,
  TableCell,
  TableRow,
  Typography,
} from "@mui/material";

import { useCatalog } from "../app/providers";
import { useApiClient } from "../api/client";
import { ContractError } from "../api/error";
import type { Row } from "../api/types";
import { canDelete, canUpdate, canViewDetail } from "../capabilities";
import { spaEditPath, spaListPath } from "../api/paths";
import { formatCell } from "../fields";

export function ResourceDetailPage() {
  const { slug = "", id = "" } = useParams();
  const { bySlug } = useCatalog();
  const model = bySlug.get(slug);
  const client = useApiClient();
  const navigate = useNavigate();

  const [row, setRow] = useState<Row | null>(null);
  const [status, setStatus] = useState("");
  const [loading, setLoading] = useState(true);
  const [forbidden, setForbidden] = useState(false);
  const [unauthorized, setUnauthorized] = useState(false);

  useEffect(() => {
    if (!model || !canViewDetail(model)) {
      setLoading(false);
      return;
    }
    let cancelled = false;
    client
      .detail(slug, id)
      .then((envelope) => {
        if (!cancelled) {
          setRow(envelope.data);
        }
      })
      .catch((err: unknown) => {
        if (cancelled) {
          return;
        }
        if (ContractError.unauthorized(err)) {
          setUnauthorized(true);
          return;
        }
        if (ContractError.forbidden(err)) {
          setForbidden(true);
          return;
        }
        setStatus(err instanceof Error ? err.message : "request failed");
      })
      .finally(() => {
        if (!cancelled) {
          setLoading(false);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [client, id, model, slug]);

  async function onDelete() {
    if (!window.confirm(`Delete this ${model?.singular ?? "row"}?`)) {
      return;
    }
    try {
      await client.remove(slug, id);
      navigate(spaListPath(slug));
    } catch (err: unknown) {
      if (ContractError.unauthorized(err)) {
        setUnauthorized(true);
        return;
      }
      if (ContractError.forbidden(err)) {
        setForbidden(true);
        return;
      }
      setStatus(err instanceof Error ? err.message : "delete failed");
    }
  }

  if (unauthorized) {
    return <Navigate to="/login" replace />;
  }
  if (!model) {
    return <Alert severity="warning">Unknown model.</Alert>;
  }
  if (!model.can.view) {
    return <Alert severity="warning">You do not have permission to view this {model.singular}.</Alert>;
  }
  if (!model.actions.detail) {
    return <Alert severity="info">Detail is disabled for {model.plural}.</Alert>;
  }
  if (forbidden) {
    return <Alert severity="warning">You do not have permission to view this {model.singular}.</Alert>;
  }
  if (loading) {
    return (
      <Box sx={{ display: "flex", justifyContent: "center", py: 6 }}>
        <CircularProgress />
      </Box>
    );
  }
  if (!row) {
    return <Alert severity="error">{status || "Not found."}</Alert>;
  }

  const names = model.fields.map((field) => field.name);
  for (const extra of ["created_at", "updated_at"]) {
    if (row[extra] !== undefined && !names.includes(extra)) {
      names.push(extra);
    }
  }

  return (
    <Box>
      <Typography variant="h4" component="h1" sx={{ mb: 1 }}>
        {model.singular}
      </Typography>
      <Box sx={{ display: "flex", gap: 1, mb: 2 }}>
        <Button component={Link} to={spaListPath(slug)}>
          Back to list
        </Button>
        {canUpdate(model) ? (
          <Button variant="contained" component={Link} to={spaEditPath(slug, id)}>
            Edit
          </Button>
        ) : null}
        {canDelete(model) ? (
          <Button color="error" onClick={() => void onDelete()}>
            Delete
          </Button>
        ) : null}
      </Box>
      {status ? (
        <Alert severity="error" sx={{ mb: 2 }}>
          {status}
        </Alert>
      ) : null}
      <Paper>
        <Table>
          <TableBody>
            {names.map((name) => (
              <TableRow key={name}>
                <TableCell width="30%">{name}</TableCell>
                <TableCell>
                  {formatCell(row[name], model.fields.find((field) => field.name === name))}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Paper>
    </Box>
  );
}
