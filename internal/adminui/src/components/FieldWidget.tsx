import { useEffect, useState } from "react";
import { Controller, type Control, type FieldValues } from "react-hook-form";

import {
  Autocomplete,
  Box,
  Checkbox,
  Chip,
  FormControl,
  FormControlLabel,
  FormLabel,
  TextField,
  Typography,
} from "@mui/material";

import { useApiClient } from "../api/client";
import { useCatalog } from "../app/providers";
import type { FieldMeta, Row } from "../api/types";
import {
  compareDecimal,
  formPattern,
  isBelongsTo,
  isHasMany,
  isManyToMany,
  type RelId,
  relationListQuery,
  relationOptions,
  toIdList,
  toRelId,
} from "../fields";

type Props = {
  field: FieldMeta;
  control: Control<FieldValues>;
  disabled?: boolean;
};

type Option = { value: string; label: string };

// relationPageSize is the related-model page fetched for the picker
// (contract.MaxPerPage). When the related model has a configured Search, typing
// narrows this page server-side, so rows beyond it are reachable; otherwise the
// widget filters the loaded page client-side and flags the truncation.
const relationPageSize = 100;

export function FieldWidget({ field, control, disabled }: Props) {
  const readOnly = disabled || field.readonly || isHasMany(field);
  const label = fieldLabel(field);

  if (isManyToMany(field)) {
    return <RelationMultiSelect field={field} control={control} disabled={disabled} />;
  }

  if (isBelongsTo(field)) {
    return <RelationSelect field={field} control={control} disabled={disabled} />;
  }

  if (isHasMany(field)) {
    // Read-only: show the related children already in the row as labeled chips.
    // This is a display, not a picker — it never lists the related model.
    return (
      <Controller
        name={field.name}
        control={control}
        render={({ field: rhf }) => <HasManyView field={field} ids={normalizeIds(rhf.value, true)} />}
      />
    );
  }

  if (field.type === "boolean") {
    return (
      <Controller
        name={field.name}
        control={control}
        render={({ field: rhf }) => (
          <FormControlLabel
            control={
              <Checkbox
                checked={Boolean(rhf.value)}
                onChange={(event) => rhf.onChange(event.target.checked)}
                disabled={readOnly}
              />
            }
            label={label}
          />
        )}
      />
    );
  }

  const inputType = inputTypeFor(field);
  const multiline = field.type === "text" || field.type === "json";

  return (
    <Controller
      name={field.name}
      control={control}
      rules={widgetRules(field, readOnly)}
      render={({ field: rhf, fieldState }) => (
        <TextField
          {...rhf}
          value={rhf.value ?? ""}
          type={inputType}
          label={label}
          fullWidth
          multiline={multiline}
          minRows={multiline ? 3 : undefined}
          error={!!fieldState.error}
          helperText={fieldState.error?.message ?? helperText(field)}
          disabled={readOnly}
          slotProps={widgetSlotProps(field)}
        />
      )}
    />
  );
}

/** useDebounced returns value after it has been stable for delayMs. */
function useDebounced(value: string, delayMs: number): string {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(timer);
  }, [value, delayMs]);
  return debounced;
}

/** normalizeIds pulls the selected primary keys off a picker's form value. */
function normalizeIds(value: unknown, multiple: boolean): RelId[] {
  if (multiple) {
    return Array.isArray(value) ? (value as RelId[]) : [];
  }
  return value == null || value === "" ? [] : [value as RelId];
}

/**
 * RelationAutocomplete is the searchable picker shared by belongs_to (single)
 * and many_to_many (multiple). It loads the related model's rows from the
 * data-plane list endpoint (client.list -> /api/v1/admin/...); when the related
 * model has a configured Search it searches server-side (so rows past the first
 * page are reachable) and leaves Autocomplete's local filter off; otherwise it
 * falls back to client-side filtering of the loaded page. Already-selected ids
 * outside the current page are fetched via client.detail so they show a label,
 * not a raw key (#223).
 */
function RelationAutocomplete({
  field,
  multiple,
  value,
  onChange,
  disabled,
  fieldState,
}: {
  field: FieldMeta;
  multiple: boolean;
  value: unknown;
  onChange: (next: unknown) => void;
  disabled?: boolean;
  fieldState?: { error?: { message?: string } };
}) {
  const client = useApiClient();
  const { bySlug } = useCatalog();
  const slug = field.related?.slug ?? "";
  const labelField = field.related?.label_field ?? "";
  const related = bySlug.get(slug);
  const pkField = related?.pk ?? "id";
  const searchable = (related?.search?.length ?? 0) > 0;

  const [input, setInput] = useState("");
  const search = useDebounced(input, 250);
  const [options, setOptions] = useState<Option[]>([]);
  const [selectedLabels, setSelectedLabels] = useState<Map<string, Option>>(new Map());
  const [truncated, setTruncated] = useState(false);
  const [loadError, setLoadError] = useState("");
  const [loading, setLoading] = useState(false);

  // Load a page of options — server-side search only when the model supports it.
  useEffect(() => {
    if (!slug) {
      return;
    }
    let cancelled = false;
    setLoading(true);
    void (async () => {
      try {
        const env = await client.list(slug, relationListQuery(searchable, search, relationPageSize));
        if (cancelled) {
          return;
        }
        const rows = Array.isArray(env.data) ? env.data : [];
        setTruncated((env.meta?.total ?? rows.length) > relationPageSize);
        setOptions(relationOptions(rows, pkField, labelField));
        setLoadError("");
      } catch (err: unknown) {
        if (!cancelled) {
          setLoadError(err instanceof Error ? err.message : "failed to load options");
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
  }, [client, slug, labelField, pkField, searchable, search]);

  const ids = normalizeIds(value, multiple);
  const optionByValue = new Map(options.map((option) => [option.value, option]));
  const idsKey = ids.map(String).join(",");

  // Fetch labels for selected ids that are not on the current page, so an
  // already-selected out-of-page row shows its label instead of its key.
  useEffect(() => {
    if (!slug) {
      return;
    }
    const missing = ids
      .map(String)
      .filter((v) => !optionByValue.has(v) && !selectedLabels.has(v));
    if (missing.length === 0) {
      return;
    }
    let cancelled = false;
    void (async () => {
      const fetched = new Map(selectedLabels);
      await Promise.all(
        missing.map(async (id) => {
          try {
            const env = await client.detail(slug, id);
            const row = env.data as Row | undefined;
            const label = row && labelField && row[labelField] != null ? String(row[labelField]) : id;
            fetched.set(id, { value: id, label });
          } catch {
            // Leave it to the key fallback below.
          }
        }),
      );
      if (!cancelled) {
        setSelectedLabels(fetched);
      }
    })();
    return () => {
      cancelled = true;
    };
    // idsKey and options track when the selected set or the page changes.
  }, [client, slug, labelField, idsKey, options]); // eslint-disable-line react-hooks/exhaustive-deps

  const resolve = (id: RelId): Option => {
    const v = String(id);
    return optionByValue.get(v) ?? selectedLabels.get(v) ?? { value: v, label: v };
  };

  const helper = loadError
    ? loadError
    : searchable
      ? "Type to search"
      : truncated
        ? `Showing first ${relationPageSize} ${slug} — refine the model's Search to reach the rest`
        : `Select related ${slug}`;

  const acValue = multiple ? ids.map(resolve) : ids.length > 0 ? resolve(ids[0]) : null;

  return (
    <Autocomplete
      multiple={multiple}
      options={options}
      value={acValue as never}
      getOptionLabel={(option) => option.label}
      isOptionEqualToValue={(option, selected) => option.value === selected.value}
      filterOptions={searchable ? (opts) => opts : undefined}
      filterSelectedOptions={multiple}
      disabled={disabled}
      loading={loading}
      onInputChange={(_event, term, reason) => {
        // Only a user keystroke (or clearing it) drives the search term; a
        // select/reset sets the input to the chosen label and must not refetch.
        if (reason === "input" || reason === "clear") {
          setInput(term);
        }
      }}
      onChange={(_event, selected) => {
        if (multiple) {
          onChange(toIdList((selected as Option[]).map((option) => option.value)));
        } else {
          const one = selected as Option | null;
          onChange(one ? toRelId(one.value) : null);
        }
      }}
      renderInput={(params) => (
        <TextField
          {...params}
          label={fieldLabel(field)}
          fullWidth
          required={!multiple && field.required && !disabled}
          error={!!fieldState?.error || !!loadError}
          helperText={fieldState?.error?.message ?? helper}
        />
      )}
    />
  );
}

/** RelationMultiSelect renders a many-to-many field as a searchable multi-select. */
function RelationMultiSelect({ field, control, disabled }: Props) {
  return (
    <Controller
      name={field.name}
      control={control}
      render={({ field: rhf }) => (
        <RelationAutocomplete
          field={field}
          multiple
          value={rhf.value}
          onChange={rhf.onChange}
          disabled={disabled || field.readonly}
        />
      )}
    />
  );
}

/** RelationSelect renders a belongs_to foreign key as a searchable single-select. */
function RelationSelect({ field, control, disabled }: Props) {
  const readOnly = disabled || field.readonly;
  return (
    <Controller
      name={field.name}
      control={control}
      rules={{ required: field.required && !readOnly }}
      render={({ field: rhf, fieldState }) => (
        <RelationAutocomplete
          field={field}
          multiple={false}
          value={rhf.value}
          onChange={rhf.onChange}
          disabled={readOnly}
          fieldState={fieldState}
        />
      )}
    />
  );
}

/**
 * HasManyView renders a has_many field's related children as read-only chips.
 * It shows the ids already in the row (never lists the related model); when the
 * related model is registered it fetches each child's label via client.detail,
 * otherwise it falls back to the raw key (#223).
 */
function HasManyView({ field, ids }: { field: FieldMeta; ids: RelId[] }) {
  const client = useApiClient();
  const { bySlug } = useCatalog();
  const slug = field.related?.slug ?? "";
  const labelField = field.related?.label_field ?? "";
  const registered = slug !== "" && bySlug.has(slug);
  const [labels, setLabels] = useState<Map<string, string>>(new Map());
  const idsKey = ids.map(String).join(",");

  useEffect(() => {
    if (!registered || !labelField || ids.length === 0) {
      return;
    }
    let cancelled = false;
    void (async () => {
      const resolved = new Map<string, string>();
      await Promise.all(
        ids.map(async (id) => {
          try {
            const env = await client.detail(slug, String(id));
            const row = env.data as Row | undefined;
            if (row && row[labelField] != null) {
              resolved.set(String(id), String(row[labelField]));
            }
          } catch {
            // Leave it to the key fallback.
          }
        }),
      );
      if (!cancelled) {
        setLabels(resolved);
      }
    })();
    return () => {
      cancelled = true;
    };
    // idsKey tracks the selected set.
  }, [client, registered, slug, labelField, idsKey]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <FormControl fullWidth>
      <FormLabel>{fieldLabel(field)}</FormLabel>
      <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.5, mt: 0.5 }}>
        {ids.length === 0 ? (
          <Typography variant="body2" color="text.secondary">
            None
          </Typography>
        ) : (
          ids.map((id) => (
            <Chip key={String(id)} size="small" label={labels.get(String(id)) ?? String(id)} />
          ))
        )}
      </Box>
    </FormControl>
  );
}

function widgetRules(field: FieldMeta, readOnly: boolean) {
  const rules: {
    required?: boolean;
    maxLength?: { value: number; message: string };
    pattern?: { value: RegExp; message: string };
    min?: { value: number; message: string };
    max?: { value: number; message: string };
    validate?: (value: unknown) => true | string;
  } = {};
  if (field.required && !readOnly) {
    rules.required = true;
  }
  if (field.max_length) {
    const limit = field.max_length;
    rules.validate = (value) => {
      if (value == null || value === "") {
        return true;
      }
      if ([...String(value)].length > limit) {
        return "is too long";
      }
      return true;
    };
  }
  if (field.pattern) {
    rules.pattern = { value: new RegExp(formPattern(field.pattern), "u"), message: "does not match pattern" };
  }
  if (field.type === "decimal" && (field.minimum || field.maximum)) {
    rules.validate = (value) => {
      if (value == null || value === "") {
        return true;
      }
      const text = String(value).trim();
      if (!/^-?\d+(\.\d+)?$/.test(text)) {
        return "must be a decimal";
      }
      if (field.minimum && compareDecimal(text, field.minimum) < 0) {
        return `must be at least ${field.minimum}`;
      }
      if (field.maximum && compareDecimal(text, field.maximum) > 0) {
        return `must be at most ${field.maximum}`;
      }
      return true;
    };
  } else {
    if (field.minimum) {
      rules.min = { value: Number(field.minimum), message: `must be at least ${field.minimum}` };
    }
    if (field.maximum) {
      rules.max = { value: Number(field.maximum), message: `must be at most ${field.maximum}` };
    }
  }
  return rules;
}

function widgetSlotProps(field: FieldMeta) {
  const htmlInput: Record<string, string | number> = {};
  if (field.minimum) {
    htmlInput.min = field.minimum;
  }
  if (field.maximum) {
    htmlInput.max = field.maximum;
  }
  if (field.type === "datetime" || field.type === "date") {
    return {
      inputLabel: { shrink: true },
      ...(Object.keys(htmlInput).length > 0 ? { htmlInput } : {}),
    };
  }
  if (Object.keys(htmlInput).length === 0) {
    return undefined;
  }
  return { htmlInput };
}

function fieldLabel(field: FieldMeta): string {
  if (field.type === "relation" && field.related) {
    if (field.related.kind === "belongs_to") {
      return `${field.name} (${field.related.slug})`;
    }
    return `${field.name} (${field.related.kind})`;
  }
  return field.name;
}

function inputTypeFor(field: FieldMeta): string {
  switch (field.type) {
    case "integer":
    case "float":
      return "number";
    // decimal stays a text input: it is submitted as an exact string, and a
    // number input would coerce it to a float.
    case "datetime":
      return "datetime-local";
    case "date":
      return "date";
    default:
      return "text";
  }
}

function helperText(field: FieldMeta): string | undefined {
  if (field.type === "json") {
    return "JSON object or array";
  }
  if (field.type === "relation" && field.related?.kind === "belongs_to") {
    return `Foreign key for ${field.related.slug}`;
  }
  if (isHasMany(field)) {
    return "has_many is meta-only";
  }
  return undefined;
}
