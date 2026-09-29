/**
 * Encode a value as one URL path segment so `/` and `..` in a string PK
 * cannot add segments or be normalized by `new URL()`.
 */
export function pathSegment(value: string): string {
  return encodeURIComponent(value);
}

export function apiMetaPath(slug: string): string {
  return `/admin/meta/${pathSegment(slug)}`;
}

export function apiResourcePath(slug: string, id?: string): string {
  const base = `/admin/resources/${pathSegment(slug)}`;
  if (id === undefined) {
    return base;
  }
  return `${base}/${pathSegment(id)}`;
}

export function apiUploadPath(slug: string, field: string): string {
  return `${apiResourcePath(slug)}/uploads/${pathSegment(field)}`;
}

export function spaListPath(slug: string): string {
  return `/${pathSegment(slug)}`;
}

export function spaNewPath(slug: string): string {
  return `${spaListPath(slug)}/new`;
}

export function spaDetailPath(slug: string, id: string): string {
  return `${spaListPath(slug)}/${pathSegment(id)}`;
}

export function spaEditPath(slug: string, id: string): string {
  return `${spaDetailPath(slug, id)}/edit`;
}
