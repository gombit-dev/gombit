import { Box, Link, Typography } from "@mui/material";

import type { FieldMeta } from "../api/types";
import { asFileValue, fileLabel } from "../files";

type Props = {
  field?: FieldMeta;
  value: unknown;
  preview?: boolean;
};

// FileCell shows a file field's value: a download link labeled with the
// filename, and for an image a thumbnail (when preview is set). A file the
// store no longer has shows as missing.
export function FileCell({ field, value, preview }: Props) {
  const file = asFileValue(value);
  if (!file) {
    return null;
  }
  if (file.missing) {
    return (
      <Typography variant="body2" color="error">
        {fileLabel(file)} (missing)
      </Typography>
    );
  }
  const label = fileLabel(file);
  const link = file.url ? (
    <Link href={file.url} target="_blank" rel="noopener noreferrer" onClick={(event) => event.stopPropagation()}>
      {label}
    </Link>
  ) : (
    <span>{label}</span>
  );
  if (field?.type === "image" && file.url && preview) {
    return (
      <Box sx={{ display: "flex", alignItems: "center", gap: 1 }}>
        <Box component="img" src={file.url} alt={label} sx={{ maxHeight: 48, maxWidth: 96, objectFit: "contain" }} />
        {link}
      </Box>
    );
  }
  return link;
}
