import { useEffect, useMemo, useState } from "react";
import { useForm, type FieldValues } from "react-hook-form";
import { Link, Navigate, useNavigate, useParams } from "react-router";

import { Alert, Box, Button, CircularProgress, Paper, Typography } from "@mui/material";

import { useCatalog } from "../app/providers";
import { useApiClient } from "../api/client";
import { applyContractErrors, unmountedContractMessage } from "../api/formErrors";
import { ContractError } from "../api/error";
import { FieldWidget } from "../components/FieldWidget";
import { canCreate, canPopulateEditForm, canUpdate, canViewDetail } from "../capabilities";
import { spaDetailPath, spaListPath } from "../api/paths";
import { emptyFormValue, formValuesToBody, rowToFormValues, writableFields } from "../fields";
import type { Row } from "../api/types";

type Props = {
  mode: "create" | "edit";
};

export function ResourceFormPage({ mode }: Props) {
  const { slug = "", id = "" } = useParams();
  const { bySlug } = useCatalog();
  const model = bySlug.get(slug);
  const client = useApiClient();
  const navigate = useNavigate();
  const [status, setStatus] = useState("");
  const [loading, setLoading] = useState(mode === "edit");
  const [rowLoaded, setRowLoaded] = useState(false);
  const [forbidden, setForbidden] = useState(false);
  const [unauthorized, setUnauthorized] = useState(false);

  const defaults = useMemo(() => {
    const values: Row = {};
    if (!model) {
      return values;
    }
    for (const field of model.fields) {
      values[field.name] = emptyFormValue(field);
    }
    return values;
  }, [model]);

  const {
    control,
    handleSubmit,
    reset,
    setError,
    formState: { isSubmitting },
  } = useForm<FieldValues>({
    defaultValues: defaults,
  });

  useEffect(() => {
    if (mode !== "edit" || !model || !canPopulateEditForm(model)) {
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setRowLoaded(false);
    client
      .detail(slug, id)
      .then((envelope) => {
        if (cancelled) {
          return;
        }
        reset(rowToFormValues(envelope.data, model.fields));
        setRowLoaded(true);
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
  }, [client, id, mode, model, reset, slug]);

  async function onSubmit(values: FieldValues) {
    if (!model) {
      return;
    }
    if (mode === "edit" && !rowLoaded) {
      return;
    }
    setStatus("");
    const { body, jsonErrors } = formValuesToBody(values, model.fields);
    if (Object.keys(jsonErrors).length > 0) {
      for (const [name, message] of Object.entries(jsonErrors)) {
        setError(name, { type: "validate", message });
      }
      return;
    }
    try {
      if (mode === "create") {
        const created = await client.create(slug, body);
        const pk = model.pk || "id";
        const createdId = created.data[pk];
        if (canViewDetail(model) && createdId !== undefined) {
          navigate(spaDetailPath(slug, String(createdId)));
        } else {
          navigate(spaListPath(slug));
        }
        return;
      }
      await client.update(slug, id, body);
      if (canViewDetail(model)) {
        navigate(spaDetailPath(slug, id));
      } else {
        navigate(spaListPath(slug));
      }
    } catch (err: unknown) {
      if (ContractError.unauthorized(err)) {
        setUnauthorized(true);
        return;
      }
      if (ContractError.forbidden(err)) {
        setForbidden(true);
        return;
      }
      const mounted = new Set(
        model.fields
          .filter((field) => mode === "edit" || writableFields([field]).length > 0)
          .map((field) => field.name),
      );
      const orphan = unmountedContractMessage(err, mounted);
      if (orphan) {
        setStatus(orphan);
      }
      if (!applyContractErrors(setError, err) && !orphan) {
        setStatus(err instanceof Error ? err.message : "request failed");
      }
    }
  }

  if (unauthorized) {
    return <Navigate to="/login" replace />;
  }
  if (!model) {
    return <Alert severity="warning">Unknown model.</Alert>;
  }
  if (mode === "create" && !model.actions.create) {
    return <Alert severity="info">Create is disabled for {model.plural}.</Alert>;
  }
  if (mode === "edit" && !model.actions.update) {
    return <Alert severity="info">Update is disabled for {model.plural}.</Alert>;
  }
  if (mode === "create" && !canCreate(model)) {
    return <Alert severity="warning">You do not have permission to create {model.plural}.</Alert>;
  }
  if (mode === "edit" && !canUpdate(model)) {
    return <Alert severity="warning">You do not have permission to edit this {model.singular}.</Alert>;
  }
  if (mode === "edit" && !canPopulateEditForm(model)) {
    return (
      <Alert severity="info">
        Edit requires viewing this {model.singular}. Detail is disabled or not permitted.
      </Alert>
    );
  }
  if (forbidden) {
    return (
      <Alert severity="warning">
        You do not have permission to {mode === "create" ? `create ${model.plural}` : `edit this ${model.singular}`}.
      </Alert>
    );
  }
  if (loading) {
    return (
      <Box sx={{ display: "flex", justifyContent: "center", py: 6 }}>
        <CircularProgress />
      </Box>
    );
  }
  if (mode === "edit" && !rowLoaded) {
    return <Alert severity="error">{status || "Not found."}</Alert>;
  }

  const fields = model.fields.filter((field) => mode === "edit" || writableFields([field]).length > 0);

  return (
    <Box>
      <Typography variant="h4" component="h1" sx={{ mb: 1 }}>
        {mode === "create" ? `New ${model.singular}` : `Edit ${model.singular}`}
      </Typography>
      <Button component={Link} to={mode === "edit" ? spaDetailPath(slug, id) : spaListPath(slug)} sx={{ mb: 2 }}>
        Back
      </Button>
      <Paper sx={{ p: 3, maxWidth: 640 }}>
        <Box
          component="form"
          onSubmit={handleSubmit(onSubmit)}
          sx={{ display: "flex", flexDirection: "column", gap: 2 }}
        >
          {fields.map((field) => (
            <FieldWidget
              key={field.name}
              field={field}
              control={control}
              disabled={field.readonly}
            />
          ))}
          <Button type="submit" variant="contained" disabled={isSubmitting}>
            {isSubmitting ? <CircularProgress size={24} color="inherit" /> : mode === "create" ? "Create" : "Save"}
          </Button>
        </Box>
        {status ? (
          <Alert severity="error" sx={{ mt: 2 }}>
            {status}
          </Alert>
        ) : null}
      </Paper>
    </Box>
  );
}
