// Package urlbase holds the rules for the URLs an app is served under, for
// the packages that must apply them without depending on each other: config
// validates GOMBIT_STORAGE_LOCAL_URL and GOMBIT_STORAGE_S3_PUBLIC_URL with
// the same functions storage/presign and storage/s3 use, so a configuration
// that validates is one they accept, and it normalises and validates
// GOMBIT_API_PREFIX with the two APIPrefix functions below, so the prefix a
// loaded configuration stores is the form its routes, its OpenAPI document
// and the client generated from that document are all built from.
package urlbase

import (
	"net/url"
	"strings"
)

// The canonical form of an API prefix (config.API.Prefix, GOMBIT_API_PREFIX):
// '/' followed by segments of letters, digits, '.', '_', '-' or '~',
// separated by a single '/' and with no trailing '/'.
//
// That form is what the value has to be. Generated resources and the tutorial
// example concatenate prefix + "/widgets" into the routes they register and
// into the OpenAPI document, while Gin cleans a doubled slash out of the route
// it registers but matches requests as sent: a prefix ending in '/' serves
// /svc/widgets and advertises /svc//widgets, so every client built from that
// document asks for a URL that answers 404. Characters a route cannot be built
// from — spaces, '%', '}' or Gin's ':' and '*' wildcards — are reported rather
// than left to produce a document no server matches, and so is a '.' or '..'
// segment, which path.Clean collapses to a prefix no route is registered under.

// CanonicalAPIPrefix returns the canonical form of an API prefix, or "" when
// it has none, which APIPrefixProblem explains.
func CanonicalAPIPrefix(prefix string) string {
	canonical, problem := apiPrefix(prefix)
	if problem != "" {
		return ""
	}
	return canonical
}

// APIPrefixProblem reports why prefix cannot be served as an API prefix, or
// "" when it can. A prefix that has a canonical form but is not already it is
// still a problem: the callers that read config.API.Prefix get the field, not
// this function, so only a canonical value reaches all of them unchanged.
func APIPrefixProblem(prefix string) string {
	canonical, problem := apiPrefix(prefix)
	switch {
	case problem != "":
		return problem
	case canonical != prefix:
		return "must not end with '/'"
	}
	return ""
}

func apiPrefix(prefix string) (canonical, problem string) {
	if !strings.HasPrefix(prefix, "/") {
		return "", "must start with /"
	}
	// A trailing '/' is all that is canonicalised away: config's loader has
	// already trimmed surrounding space, and a space inside a prefix is a
	// mistake to report, not to hide.
	canonical = strings.TrimRight(prefix, "/")
	if canonical == "" {
		return "", `must not be empty or only '/'`
	}
	for _, segment := range strings.Split(canonical[1:], "/") {
		if segment == "." || segment == ".." {
			return "", `must not contain a '.' or '..' segment`
		}
		if !isPrefixSegment(segment) {
			return "", `must be '/' followed by segments of a-z, A-Z, 0-9, . _ - ~ separated by a single '/'`
		}
	}
	return canonical, ""
}

func isPrefixSegment(segment string) bool {
	if segment == "" {
		return false
	}
	for _, r := range segment {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '~':
		default:
			return false
		}
	}
	return true
}

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
