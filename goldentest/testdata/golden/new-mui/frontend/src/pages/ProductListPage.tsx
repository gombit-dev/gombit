import { useEffect, useState } from "react";
import { Link } from "react-router";

import AddIcon from "@mui/icons-material/Add";
import {
  Alert,
  Box,
  Button,
  CircularProgress,
  Paper,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from "@mui/material";

import { useApiClient } from "../api/client";
import { unwrap } from "../api/generated/client";
import type { paths } from "../api/generated/schema";

type ListResponse =
  paths["/api/v1/products"]["get"]["responses"][200]["content"]["application/json"];
type ProductRow = NonNullable<ListResponse["data"]>[number];

export function ProductListPage() {
  const client = useApiClient();
  const [rows, setRows] = useState<ProductRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [status, setStatus] = useState("");

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const listed = await unwrap(await client.GET("/api/v1/products"));
        if (cancelled) {
          return;
        }
        const data = Array.isArray(listed.data) ? listed.data : [];
        setRows(data);
        setStatus(data.length === 0 ? "No products yet." : "");
      } catch (err: unknown) {
        if (cancelled) {
          return;
        }
        setStatus(err instanceof Error ? err.message : "request failed");
      } finally {
        if (!cancelled) {
          setLoading(false);
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [client]);

  return (
    <Box>
      <Box sx={{ display: "flex", justifyContent: "space-between", alignItems: "center", mb: 2 }}>
        <Typography variant="h4" component="h1">
          Products
        </Typography>
        <Button variant="contained" component={Link} to="/products/new" startIcon={<AddIcon />}>
          New product
        </Button>
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
                <TableCell>id</TableCell>
                <TableCell>name</TableCell>
                <TableCell>price</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {rows.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={3} align="center">
                    {status || "No products yet."}
                  </TableCell>
                </TableRow>
              ) : (
                rows.map((row) => (
                  <TableRow key={String(row.id)}>
                    <TableCell>{String(row.id)}</TableCell>
                    <TableCell>{String(row.name)}</TableCell>
                    <TableCell>{String(row.price)}</TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
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
