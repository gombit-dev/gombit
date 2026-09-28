package commandgen

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
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
	"internal": {}, "main": {}, "testdata": {}, "vendor": {}, "product": {},
	"web": {},
}

var reservedCommands = map[string]struct{}{
	"new": {}, "dev": {}, "build": {}, "make": {}, "db": {}, "openapi": {}, "client": {},
	"routes": {}, "doctor": {}, "config": {}, "createsuperuser": {}, "version": {},
	"worker": {}, "jobs": {},
	"help": {}, "completion": {},
	"gombit": {}, "register": {},
}

// CommandName is the derived identifiers for one generated management command.
type CommandName struct {
	Input       string
	Use         string
	Constructor string
	FileBase    string
	Package     string
}

func parseCommandName(raw, pkg string) (CommandName, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return CommandName{}, fmt.Errorf("commandgen: command name is required")
	}
	if strings.ContainsAny(raw, `/\`) || strings.Contains(raw, "..") {
		return CommandName{}, fmt.Errorf("commandgen: command name %q must be a single identifier", raw)
	}
	for _, r := range raw {
		if unicode.IsSpace(r) {
			return CommandName{}, fmt.Errorf("commandgen: command name %q must not contain whitespace", raw)
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
			return CommandName{}, fmt.Errorf("commandgen: command name %q contains invalid character %q", raw, string(r))
		}
	}
	first, _ := utf8.DecodeRuneInString(strings.TrimLeft(raw, "_-"))
	if first == utf8.RuneError || unicode.IsDigit(first) {
		return CommandName{}, fmt.Errorf("commandgen: command name %q must start with a letter", raw)
	}

	typeName := toPascal(raw)
	if typeName == "" || !isExportedIdent(typeName) {
		return CommandName{}, fmt.Errorf("commandgen: command name %q is not a valid exported Go identifier", raw)
	}
	fileBase := toSnake(typeName)
	use := strings.ReplaceAll(fileBase, "_", "-")
	if _, reserved := reservedCommands[use]; reserved {
		return CommandName{}, fmt.Errorf("commandgen: command name %q collides with a framework command", raw)
	}
	if fileBase == "register" || fileBase == "commands" {
		return CommandName{}, fmt.Errorf("commandgen: command name %q maps to reserved file %s.go", raw, fileBase)
	}

	pkg = strings.TrimSpace(pkg)
	if pkg == "" {
		pkg = defaultPackage
	}
	if _, reserved := reservedPackages[pkg]; reserved {
		return CommandName{}, fmt.Errorf("commandgen: package %q is reserved", pkg)
	}
	if _, kw := goKeywords[pkg]; kw {
		return CommandName{}, fmt.Errorf("commandgen: package %q is a Go keyword", pkg)
	}
	if !isGoIdent(pkg) {
		return CommandName{}, fmt.Errorf("commandgen: package %q is not a valid Go identifier", pkg)
	}

	return CommandName{
		Input:       raw,
		Use:         use,
		Constructor: "New" + typeName + "Command",
		FileBase:    fileBase,
		Package:     pkg,
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
		r, size := utf8.DecodeRuneInString(part)
		b.WriteRune(unicode.ToUpper(r))
		b.WriteString(strings.ToLower(part[size:]))
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
