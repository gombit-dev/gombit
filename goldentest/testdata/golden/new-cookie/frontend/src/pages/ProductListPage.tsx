import { useEffect, useState } from "react";
import { Link } from "react-router";

import { useApiClient } from "../api/client";
import { unwrap } from "../api/generated/client";
import type { paths } from "../api/generated/schema";

type ListResponse =
  paths["/api/v1/products"]["get"]["responses"][200]["content"]["application/json"];
type ProductRow = NonNullable<ListResponse["data"]>[number];

export function ProductListPage() {
  const client = useApiClient();
  const [rows, setRows] = useState<ProductRow[]>([]);
  const [status, setStatus] = useState("Loading products…");

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
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [client]);

  return (
    <section>
      <h1>Products</h1>
      <p>
        <Link to="/products/new">New product</Link>
      </p>
      <table>
        <thead>
          <tr>
            <th>id</th>
            <th>name</th>
            <th>price</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={String(row.id)}>
              <td>{String(row.id)}</td>
              <td>{String(row.name)}</td>
              <td>{String(row.price)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {status ? <p>{status}</p> : null}
    </section>
  );
}
