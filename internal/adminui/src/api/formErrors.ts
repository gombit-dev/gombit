import type { FieldValues, Path, UseFormSetError } from "react-hook-form";

import { ContractError, isD10ErrorBody } from "./error";

/**
 * Map a D10 `error.fields` payload onto React Hook Form field errors.
 * Accepts `ContractError` or a D10 error body. Returns true when at least
 * one field error was set.
 */
export function applyContractErrors<TFieldValues extends FieldValues>(
  setError: UseFormSetError<TFieldValues>,
  err: unknown,
): boolean {
  const fields = d10Fields(err);
  if (fields === undefined) {
    return false;
  }
  let applied = false;
  for (const [name, messages] of Object.entries(fields)) {
    if (name === "") {
      continue;
    }
    const message = (messages ?? []).filter((item) => item.trim() !== "").join("; ");
    if (message === "") {
      continue;
    }
    setError(name as Path<TFieldValues>, { type: "server", message });
    applied = true;
  }
  return applied;
}

/**
 * Form-level text for D10 field errors whose names are not mounted inputs.
 * A create form does not render read-only or hidden server columns, so
 * those 422s would otherwise be swallowed when applyContractErrors returns
 * true.
 */
export function unmountedContractMessage(err: unknown, mounted: ReadonlySet<string>): string {
  const fields = d10Fields(err);
  if (fields === undefined) {
    return "";
  }
  const lines: string[] = [];
  for (const [name, messages] of Object.entries(fields)) {
    if (name === "" || mounted.has(name)) {
      continue;
    }
    const message = (messages ?? []).filter((item) => item.trim() !== "").join("; ");
    if (message === "") {
      continue;
    }
    lines.push(`${name}: ${message}`);
  }
  return lines.join("\n");
}

function d10Fields(err: unknown): { [key: string]: string[] } | undefined {
  if (err instanceof ContractError) {
    return err.fields;
  }
  if (isD10ErrorBody(err)) {
    return err.error.fields;
  }
  return undefined;
}
