// Package urlbase holds the rules for the URLs storage serves objects
// under, for the packages that must apply them without depending on each
// other: config validates GOMBIT_STORAGE_LOCAL_URL and
// GOMBIT_STORAGE_S3_PUBLIC_URL with the same functions storage/presign
// and storage/s3 use, so a configuration that validates is one they
// accept.
package urlbase

import (
	"net/url"
	"strings"
)

// Base checks where a store's URLs are served (presign.Config.Base): a
// path ("/_storage") or an http(s) URL with one
// ("https://files.example.com/_storage"), without a query, fragment,
// credentials, or trailing '/', whose path is plain (no percent-escapes:
// the path is matched against requests as decoded, so it must be the same
// written either way). It returns the path, or why base is not one.
func Base(base string) (path, problem string) {
	u, problem := parse(base)
	switch {
	case problem != "":
		return "", problem
	case !u.IsAbs() && (u.Host != "" || !strings.HasPrefix(base, "/")):
		return "", "a relative URL must be a path starting with '/'"
	case u.Path == "" || u.EscapedPath() != u.Path:
		return "", "needs a plain path to serve under, such as /_storage (no percent-escapes)"
	}
	return u.Path, ""
}

// Root checks the URL a bucket's public objects are read from
// (s3.Config.PublicURL): an http(s) URL, with or without a path, without a
// query, fragment, credentials, or trailing '/'. It returns why root is
// not one, or "".
func Root(root string) string {
	u, problem := parse(root)
	switch {
	case problem != "":
		return problem
	case !u.IsAbs():
		return "must be an absolute http(s) URL"
	case u.EscapedPath() != u.Path:
		return "needs a plain path (no percent-escapes)"
	}
	return ""
}

// parse checks what Base and Root share.
func parse(raw string) (*url.URL, string) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, err.Error()
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.HasSuffix(raw, "/") || u.User != nil:
		return nil, "has a query, fragment, credentials, or trailing '/'"
	case u.IsAbs() && (u.Scheme != "http" && u.Scheme != "https" || u.Host == ""):
		return nil, "an absolute URL must be http(s)://host[/path]"
	}
	return u, ""
}
