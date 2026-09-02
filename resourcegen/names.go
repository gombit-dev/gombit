package resourcegen

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jinzhu/inflection"
)

var goKeywords = map[string]struct{}{
	"break": {}, "case": {}, "chan": {}, "const": {}, "continue": {},
	"default": {}, "defer": {}, "else": {}, "fallthrough": {}, "for": {},
	"func": {}, "go": {}, "goto": {}, "if": {}, "import": {}, "interface": {},
	"map": {}, "package": {}, "range": {}, "return": {}, "select": {},
	"struct": {}, "switch": {}, "type": {}, "var": {},
}

var reservedPackages = map[string]struct{}{
	"platform": {}, "cmd": {}, "config": {}, "database": {}, "frontend": {},
	"internal": {}, "main": {}, "testdata": {}, "vendor": {}, "commands": {},
	"web": {}, "product": {},
}

var reservedFields = map[string]struct{}{
	"id": {}, "created_at": {}, "updated_at": {}, "deleted_at": {},
}

// ResourceName is the derived identifiers for one generated feature-package.
type ResourceName struct {
	Input       string
	TypeName    string
	Package     string
	FileBase    string
	PluralSnake string
	HTTPPath    string
	Tag         string
	Kebab       string
}

func parseResourceName(raw string) (ResourceName, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ResourceName{}, fmt.Errorf("resourcegen: resource name is required")
	}
	if strings.ContainsAny(raw, `/\`) || strings.Contains(raw, "..") {
		return ResourceName{}, fmt.Errorf("resourcegen: resource name %q must be a single identifier", raw)
	}
	for _, r := range raw {
		if unicode.IsSpace(r) {
			return ResourceName{}, fmt.Errorf("resourcegen: resource name %q must not contain whitespace", raw)
		}
		if !isASCIILetterOrDigit(r) && r != '_' && r != '-' {
			return ResourceName{}, fmt.Errorf("resourcegen: resource name %q contains invalid character %q (only ASCII letters, digits, \"_\", and \"-\" are allowed)", raw, string(r))
		}
	}
	first, _ := utf8.DecodeRuneInString(strings.TrimLeft(raw, "_-"))
	if first == utf8.RuneError || unicode.IsDigit(first) {
		return ResourceName{}, fmt.Errorf("resourcegen: resource name %q must start with a letter", raw)
	}

	typeName := toPascal(raw)
	if typeName == "" || !isExportedIdent(typeName) {
		return ResourceName{}, fmt.Errorf("resourcegen: resource name %q is not a valid exported Go type", raw)
	}
	pkg := toSnake(typeName)
	if _, reserved := reservedPackages[pkg]; reserved {
		return ResourceName{}, fmt.Errorf("resourcegen: resource name %q maps to reserved package %q", raw, pkg)
	}
	if _, kw := goKeywords[pkg]; kw {
		return ResourceName{}, fmt.Errorf("resourcegen: resource name %q maps to Go keyword %q", raw, pkg)
	}
	if !isGoIdent(pkg) {
		return ResourceName{}, fmt.Errorf("resourcegen: resource name %q maps to invalid package %q", raw, pkg)
	}

	plural := pluralizeSnake(pkg)
	kebab := strings.ReplaceAll(plural, "_", "-")
	return ResourceName{
		Input:       raw,
		TypeName:    typeName,
		Package:     pkg,
		FileBase:    pkg,
		PluralSnake: plural,
		HTTPPath:    "/" + kebab,
		Tag:         toPascal(plural),
		Kebab:       kebab,
	}, nil
}

func toPascal(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "-", "_")
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "_") {
		r, size := utf8.DecodeRuneInString(s)
		return string(unicode.ToUpper(r)) + s[size:]
	}
	parts := strings.Split(s, "_")
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		switch strings.ToLower(part) {
		case "id":
			b.WriteString("ID")
		case "url":
			b.WriteString("URL")
		default:
			r, size := utf8.DecodeRuneInString(part)
			b.WriteRune(unicode.ToUpper(r))
			b.WriteString(strings.ToLower(part[size:]))
		}
	}
	return b.String()
}

func toSnake(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "-", "_")
	if s == "" {
		return ""
	}
	if strings.Contains(s, "_") {
		return strings.ToLower(s)
	}
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		if unicode.IsUpper(r) {
			if i > 0 {
				prevUpper := unicode.IsUpper(runes[i-1])
				nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
				if !prevUpper || nextLower {
					b.WriteByte('_')
				}
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// pluralizeSnake pluralizes a snake_case identifier using GORM's inflection
// library — the same pluralizer GORM's default NamingStrategy applies to derive
// a table name (inflection.Plural(toDBName(struct))). Sharing one source keeps
// the generated route surface (and TS client method names) in agreement with
// the schema for irregular nouns: Mouse -> /mice (table mice), Person ->
// /people (table people), Analysis -> /analyses (table analyses). See #225.
func pluralizeSnake(snake string) string {
	if snake == "" {
		return "s"
	}
	return inflection.Plural(snake)
}

// isASCIILetterOrDigit reports whether r is an ASCII letter or digit.
// Unlike unicode.IsLetter/IsDigit, this rejects non-ASCII letters and digits
// ("é", full-width "１"): a resource name becomes both a Go package name and
// an import path segment (internal/<pkg>), and while Go package names may be
// Unicode, Go import paths may not. Accepting a rune here that later fails
// import-path validation lets `make resource` write the whole feature tree,
// edit main.go and database.go, and only fail deep inside makemigrations —
// after which the tree no longer compiles and every later `make resource`
// is wedged on the same invalid model (issue #217).
func isASCIILetterOrDigit(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isGoIdent(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if i == 0 {
			if !unicode.IsLetter(r) && r != '_' {
				return false
			}
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}

func isExportedIdent(name string) bool {
	if !isGoIdent(name) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}
