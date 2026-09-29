import type { ReactNode } from "react";

import type { FieldMeta } from "../api/types";
import { formatCell } from "../fields";
import { isFileField } from "../files";
import { FileCell } from "./FileCell";

// cellValue renders a value in a list or detail table: a file field as a
// download link (with a thumbnail for an image when preview is set),
// anything else as formatCell's text.
export function cellValue(value: unknown, field?: FieldMeta, preview = false): ReactNode {
  if (isFileField(field)) {
    return <FileCell field={field} value={value} preview={preview} />;
  }
  return formatCell(value, field);
}
