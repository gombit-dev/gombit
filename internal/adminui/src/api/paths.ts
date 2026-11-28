/**
 * Encode a value as one URL path segment so a `/` in a string PK cannot add
 * segments. Dots are left alone by `encodeURIComponent`: `.` and `..` are dot
 * segments that a URL parser removes before the request is sent, which is why
 * the admin refuses such a key rather than trying to encode it.
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
