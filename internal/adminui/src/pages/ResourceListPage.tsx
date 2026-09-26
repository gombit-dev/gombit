import { useEffect, useMemo, useState } from "react";
import { Link, Navigate, useNavigate, useParams } from "react-router";

import AddIcon from "@mui/icons-material/Add";
import {
  Alert,
  Box,
  Button,
  CircularProgress,
  MenuItem,
  Paper,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TablePagination,
  TableRow,
  TextField,
  Typography,
} from "@mui/material";

import { useCatalog } from "../app/providers";
import { useApiClient } from "../api/client";
import { ContractError } from "../api/error";
import type { Row } from "../api/types";
import { canCreate, canList, canViewDetail } from "../capabilities";
import { spaDetailPath, spaNewPath } from "../api/paths";
import { formatCell } from "../fields";

export function ResourceListPage() {
  const { slug = "" } = useParams();
  const { bySlug } = useCatalog();
  const model = bySlug.get(slug);
  const client = useApiClient();
  const navigate = useNavigate();

  const columns = useMemo(() => {
    if (!model) {
      return [];
    }
    return model.list.length > 0 ? model.list : model.fields.map((field) => field.name);
  }, [model]);

  const [rows, setRows] = useState<Row[]>([]);
  const [page, setPage] = useState(0);
  const [perPage, setPerPage] = useState(20);
  const [total, setTotal] = useState(0);
  const [search, setSearch] = useState("");
  const [searchDraft, setSearchDraft] = useState("");
  const [ordering, setOrdering] = useState("");
  const [filters, setFilters] = useState<Record<string, string>>({});
  const [filterDraft, setFilterDraft] = useState<Record<string, string>>({});
  const [loading, setLoading] = useState(true);
  const [status, setStatus] = useState("");
  const [forbidden, setForbidden] = useState(false);
  const [unauthorized, setUnauthorized] = useState(false);

  useEffect(() => {
    if (!model || !canList(model)) {
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    const query: Record<string, string | number | undefined> = {
      page: page + 1,
      per_page: perPage,
    };
    if (model.search.length > 0 && search) {
      query.search = search;
    }
    if (ordering) {
      query.ordering = ordering;
    }
    for (const key of model.filter) {
      if (filters[key]) {
        query[key] = filters[key];
      }
    }
    client
      .list(slug, query)
      .then((envelope) => {
        if (cancelled) {
          return;
        }
        setRows(Array.isArray(envelope.data) ? envelope.data : []);
        setTotal(envelope.meta?.total ?? 0);
        setStatus("");
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
  }, [client, filters, model, ordering, page, perPage, search, slug]);

  if (unauthorized) {
    return <Navigate to="/login" replace />;
  }
  if (!model) {
    return <Alert severity="warning">Unknown model.</Alert>;
  }
  if (forbidden) {
    return <Alert severity="warning">You do not have permission to list {model.plural}.</Alert>;
  }
  if (!model.can.view) {
    return <Alert severity="warning">You do not have permission to list {model.plural}.</Alert>;
  }
  if (!model.actions.list) {
    return <Alert severity="info">Listing is disabled for {model.plural}.</Alert>;
  }

  const pk = model.pk || "id";

  return (
    <Box>
      <Box sx={{ display: "flex", justifyContent: "space-between", alignItems: "center", mb: 2, gap: 2 }}>
        <Typography variant="h4" component="h1">
          {model.plural}
        </Typography>
        {canCreate(model) ? (
          <Button variant="contained" component={Link} to={spaNewPath(slug)} startIcon={<AddIcon />}>
            New {model.singular}
          </Button>
        ) : null}
      </Box>

      <Box sx={{ display: "flex", flexWrap: "wrap", gap: 2, mb: 2 }}>
        {model.search.length > 0 ? (
          <Box
            component="form"
            onSubmit={(event) => {
              event.preventDefault();
              setPage(0);
              setSearch(searchDraft);
            }}
            sx={{ display: "flex", gap: 1 }}
          >
            <TextField
              size="small"
              label="Search"
              value={searchDraft}
              onChange={(event) => setSearchDraft(event.target.value)}
            />
            <Button type="submit" variant="outlined">
              Search
            </Button>
          </Box>
        ) : null}
        {model.ordering.length > 0 ? (
          <TextField
            select
            size="small"
            label="Ordering"
            value={ordering}
            onChange={(event) => {
              setPage(0);
              setOrdering(event.target.value);
            }}
            sx={{ minWidth: 180 }}
          >
            <MenuItem value="">Default</MenuItem>
            {model.ordering.flatMap((field) => [
              <MenuItem key={field} value={field}>
                {field} ↑
              </MenuItem>,
              <MenuItem key={`-${field}`} value={`-${field}`}>
                {field} ↓
              </MenuItem>,
            ])}
          </TextField>
        ) : null}
        {model.filter.length > 0 ? (
          <Box
            component="form"
            onSubmit={(event) => {
              event.preventDefault();
              setPage(0);
              setFilters({ ...filterDraft });
            }}
            sx={{ display: "flex", flexWrap: "wrap", gap: 1 }}
          >
            {model.filter.map((key) => (
              <TextField
                key={key}
                size="small"
                label={`Filter ${key}`}
                value={filterDraft[key] ?? ""}
                onChange={(event) => {
                  setFilterDraft((current) => ({ ...current, [key]: event.target.value }));
                }}
              />
            ))}
            <Button type="submit" variant="outlined">
              Apply
            </Button>
          </Box>
        ) : null}
      </Box>

      {loading ? (
        <Box sx={{ display: "flex", justifyContent: "center", py: 6 }}>
          <CircularProgress />
        </Box>
      ) : (
        <TableContainer component={Paper}>
          <Table>
            <TableHead>
              <TableRow>
                {columns.map((column) => (
                  <TableCell key={column}>{column}</TableCell>
                ))}
              </TableRow>
            </TableHead>
            <TableBody>
              {rows.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={Math.max(columns.length, 1)} align="center">
                    {status || "No rows."}
                  </TableCell>
                </TableRow>
              ) : (
                rows.map((row) => {
                  const id = formatCell(row[pk]);
                  return (
                    <TableRow
                      key={id || JSON.stringify(row)}
                      hover={Boolean(canViewDetail(model) && id)}
                      sx={canViewDetail(model) && id ? { cursor: "pointer" } : undefined}
                      onClick={
                        canViewDetail(model) && id ? () => navigate(spaDetailPath(slug, id)) : undefined
                      }
                    >
                      {columns.map((column) => (
                        <TableCell key={column}>
                          {formatCell(row[column], model.fields.find((field) => field.name === column))}
                        </TableCell>
                      ))}
                    </TableRow>
                  );
                })
              )}
            </TableBody>
          </Table>
          <TablePagination
            component="div"
            count={total}
            page={page}
            onPageChange={(_, next) => setPage(next)}
            rowsPerPage={perPage}
            onRowsPerPageChange={(event) => {
              setPerPage(Number.parseInt(event.target.value, 10));
              setPage(0);
            }}
            rowsPerPageOptions={[10, 20, 50, 100]}
          />
        </TableContainer>
      )}
      {!loading && status && rows.length > 0 ? (
        <Alert severity="error" sx={{ mt: 2 }}>
          {status}
        </Alert>
      ) : null}
    </Box>
  );
}
