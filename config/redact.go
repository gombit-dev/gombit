package config

import (
	"net/url"
	"sort"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// RedactedSecret is the placeholder used in place of passwords and DSN userinfo.
const RedactedSecret = "*****"

// asciiSpace is the byte class libpq and pgconn separate keyword/value tokens
// on. Go's regexp \s leaves the vertical tab out, so a class written for it
// would end a value where the driver has not.
const asciiSpace = " \t\n\r\v\f"

// secretConnKeys are the parameter names a connection string carries a password
// under. pgconn treats two of them as secrets, after it has decoded and
// canonicalised the key: `password`, which becomes Config.Password, and
// `sslpassword`, which decrypts a client key. The rest are spellings a person
// writes for the same value; they are hidden fail-closed, not because a driver
// sends them as a password. `passfile` is deliberately absent: it names the file
// to read a password from, not a secret.
var secretConnKeys = map[string]struct{}{
	"password":    {},
	"sslpassword": {},
	"passwd":      {},
	"pwd":         {},
	"pass":        {},
}

// Redacted returns a copy of c with secret-bearing fields replaced so it is
// safe to print (DSN userinfo/passwords and Redis password).
func (c Config) Redacted() Config {
	out := c
	out.Database.DSN = RedactDSN(c.Database.DSN)
	if strings.TrimSpace(c.Cache.Redis.Password) != "" {
		out.Cache.Redis.Password = RedactedSecret
	}
	if strings.TrimSpace(c.Auth.JWTSecret) != "" {
		out.Auth.JWTSecret = RedactedSecret
	}
	if strings.TrimSpace(c.Storage.URLSecret) != "" {
		out.Storage.URLSecret = RedactedSecret
	}
	if strings.TrimSpace(c.Storage.S3.SecretAccessKey) != "" {
		out.Storage.S3.SecretAccessKey = RedactedSecret
	}
	return out
}

// RedactDSN returns dsn with every range a driver reads a password out of
// replaced: the password-like keys of a libpq keyword/value string and of a
// parameter list, the userinfo password of a connection URI, and the userinfo
// password of a MySQL net DSN. File paths without those keys are unchanged.
//
// Each reading is taken over the unmodified dsn and the ranges it names are
// merged before substitution, so no reading can consume a separator another one
// needs. Where two grammars read one string, both of its passwords are hidden,
// and where they disagree about where a value ends the wider range wins, which
// redacts past the end of the narrower password (#486).
func RedactDSN(dsn string) string {
	trimmed := strings.TrimSpace(dsn)
	if trimmed == "" {
		return dsn
	}
	return cutRanges(trimmed, passwordRanges(trimmed))
}

// grammar says which parser reads a secret range, so the spellings derived
// from it are the ones that parser applies. Applying one grammar's rules to
// another's value redacts bytes no driver sends: unquoting a MySQL password
// turned `'p'` into `p`, and a message then lost every `p` it had (#486).
type grammar uint8

const (
	// keywordGrammar is a libpq keyword/value string as pgconn reads it: a
	// value may be quoted and both readings unescape backslashes.
	keywordGrammar grammar = iota
	// mysqlGrammar is go-sql-driver/mysql, which reads its userinfo by index
	// and applies no quoting or escaping at all.
	mysqlGrammar
	// uriGrammar is a connection URI: userinfo and parameters are percent
	// decoded, and the decoded bytes are what the driver sends.
	uriGrammar
)

// secretRange is one byte range a driver reads a password out of, and the
// grammar that reads it. driver says whether a driver itself reads the key —
// `password` and `sslpassword`, spelled exactly, are all pgconn reads, and a
// look-alike such as `pwd=` or `PASSWORD=` is hidden fail-closed in the DSN
// but spelled no further: a spelling for a value no driver sends is pure
// over-redaction (#486).
type secretRange struct {
	start, end int
	grammar    grammar
	// driver reports that a driver's own parser stores this range's value as a
	// password, so a message built from the connection attempt can quote it and
	// a spelling of it has to be replaced. It is a claim about the parser, not
	// about a live connection: dsnSecretValues reads the value the way that
	// parser reads it, and a range it refuses or does not treat as a password
	// spells nothing.
	driver bool
}

// passwordRanges returns the byte ranges of dsn that a driver reads as a
// password, with the overlapping ones merged into the placeholders they become.
func passwordRanges(dsn string) [][2]int {
	ranges := secretRanges(dsn)
	pairs := make([][2]int, len(ranges))
	for i, r := range ranges {
		pairs[i] = [2]int{r.start, r.end}
	}
	return mergeRanges(pairs)
}

func secretRanges(dsn string) []secretRange {
	var out []secretRange
	out = append(out, driverKeywordRanges(dsn)...)
	out = append(out, tolerantKeywordRanges(dsn)...)
	if r, ok := mysqlPassword(dsn); ok {
		out = append(out, r)
	}
	uri := uriRanges(dsn)
	if query := paramListStart(dsn); query >= 0 {
		uri = append(uri, paramRanges(dsn, query+1)...)
	}
	// pgconn answers with a URI's settings or with none: the first component its
	// parser refuses fails the whole parse, so a URI it rejects stores no
	// password, from its userinfo or its query. The ranges still describe bytes
	// an operator wrote a secret into — RedactDSN hides them — but no spelling of
	// them is derived, because the driver never stores one to send. The line is
	// the parser's, not ParseConfig's: a URI that parses but fails a later check
	// (`?password=status` names a port-shaped value) stores its password all the
	// same, and is spelled as one. Syntactic acceptance is the stable boundary;
	// semantic validation changes with every pgconn release.
	if isPostgresScheme(dsn) && pgURIRejected(dsn) {
		for i := range uri {
			uri[i].driver = false
		}
	}
	return append(out, uri...)
}

// pgURIRejected reports whether pgconn's parseURLSettings refuses connString
// outright, mirroring the reject conditions it reaches before it stores any
// setting. It is mirrored by hand rather than asked of ParseConfig for the same
// reason mysqlPassword asks ParseDSN and this does not: ParseConfig opens the
// files a passfile or ssl setting names. TestPgURIRejectedAgreesWithTheDriver
// pins the mirror to the parser, subtest by subtest, so a rule pgconn changes
// cannot drift out of it silently. The rules followed here are pgconn's own, in
// its order — a NUL anywhere; a userinfo, host list, port list or database name
// whose percent-escapes it cannot decode; a malformed IPv6 host; and a query
// pair with no '=' or with a second raw one, or a key or value it cannot
// decode. Anything it accepts is read the way paramRanges and uriRanges already
// describe.
func pgURIRejected(connString string) bool {
	if strings.IndexByte(connString, 0) >= 0 {
		return true
	}
	rest, ok := strings.CutPrefix(connString, "postgresql://")
	if !ok {
		if rest, ok = strings.CutPrefix(connString, "postgres://"); !ok {
			return true // pgconn reads no URI under another scheme.
		}
	}

	// The userinfo runs to the first '@' provided no '/' comes before it, and
	// both halves are decoded before they are stored.
	if i := strings.IndexAny(rest, "@/"); i >= 0 && rest[i] == '@' {
		user, password, hasPassword := strings.Cut(rest[:i], ":")
		rest = rest[i+1:]
		if user != "" && pgURIDecodeFails(user) {
			return true
		}
		if hasPassword && password != "" && pgURIDecodeFails(password) {
			return true
		}
	}

	// The host and port lists are decoded as units, commas and all.
	var hosts, ports strings.Builder
	for {
		if peekByte(rest) == '[' {
			end := strings.IndexByte(rest, ']')
			if end < 0 || end == 1 {
				return true
			}
			hosts.WriteString(rest[1:end])
			rest = rest[end+1:]
			if c := peekByte(rest); c != 0 && c != ':' && c != '/' && c != '?' && c != ',' {
				return true
			}
		} else {
			i := strings.IndexAny(rest, ":/?,")
			if i < 0 {
				i = len(rest)
			}
			hosts.WriteString(rest[:i])
			rest = rest[i:]
		}
		if peekByte(rest) == ':' {
			rest = rest[1:]
			i := strings.IndexAny(rest, "/?,")
			if i < 0 {
				i = len(rest)
			}
			ports.WriteString(rest[:i])
			rest = rest[i:]
		}
		if peekByte(rest) != ',' {
			break
		}
		rest = rest[1:]
		hosts.WriteByte(',')
		ports.WriteByte(',')
	}
	if hosts.Len() > 0 && pgURIDecodeFails(hosts.String()) {
		return true
	}
	if ports.Len() > 0 && pgURIDecodeFails(ports.String()) {
		return true
	}

	if peekByte(rest) == '/' {
		database := rest[1:]
		rest = ""
		if i := strings.IndexByte(database, '?'); i >= 0 {
			rest = database[i:]
			database = database[:i]
		}
		if database != "" && pgURIDecodeFails(database) {
			return true
		}
	}

	if peekByte(rest) == '?' {
		for params := rest[1:]; params != ""; {
			pair := params
			if i := strings.IndexByte(params, '&'); i >= 0 {
				pair, params = params[:i], params[i+1:]
			} else {
				params = ""
			}
			key, value, found := strings.Cut(pair, "=")
			if !found || strings.IndexByte(value, '=') >= 0 {
				return true
			}
			if pgURIDecodeFails(key) || pgURIDecodeFails(value) {
				return true
			}
		}
	}
	return false
}

// pgURIDecode is pgconn's uriDecode: the bytes a URI component stands for, as
// the driver stores them. Leading and trailing raw spaces are skipped before
// anything is decoded, so `password ` is the key `password` and `s3cr3t ` is the
// password `s3cr3t`; a '%' must be followed by two hex digits; an escape
// decoding to a NUL and a space between two runs of bytes are both refused. ok
// is false exactly where uriDecode returns an error; no other byte is a failure.
//
// This is the one decoder for URI components here. A second hand-written rule —
// matching a key with url.PathUnescape, say, which keeps the spaces pgconn
// drops — reopens the hole it closes: the key still names a password to the
// driver, so the driver puts that password in an error message, and a spelling
// derived from a component the decoder misread is not in the message
// (#486 review).
func pgURIDecode(raw string) (decoded string, ok bool) {
	i := 0
	for i < len(raw) && raw[i] == ' ' {
		i++
	}
	var b strings.Builder
	b.Grow(len(raw))
	for i < len(raw) && raw[i] != ' ' {
		if raw[i] != '%' {
			b.WriteByte(raw[i])
			i++
			continue
		}
		if i+2 >= len(raw) {
			return "", false
		}
		hi, okHi := hexDigit(raw[i+1])
		lo, okLo := hexDigit(raw[i+2])
		if !okHi || !okLo {
			return "", false
		}
		if hi<<4|lo == 0 {
			return "", false
		}
		b.WriteByte(hi<<4 | lo)
		i += 3
	}
	for i < len(raw) && raw[i] == ' ' {
		i++
	}
	if i < len(raw) {
		return "", false
	}
	return b.String(), true
}

// pgURIDecodeFails reports that pgURIDecode refuses a URI component. It is the
// question pgURIRejected asks: a component the parser refuses fails the whole
// parse, so nothing of that DSN reaches a message.
func pgURIDecodeFails(raw string) bool {
	_, ok := pgURIDecode(raw)
	return !ok
}

// hexDigit is pgconn's own: '0'-'9' and both cases of 'a'-'f' stand for a
// nibble, and nothing else does.
func hexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// peekByte is pgconn's: the first byte of s, or 0 when s is empty — safe
// because a literal NUL is refused before any of this runs.
func peekByte(s string) byte {
	if len(s) == 0 {
		return 0
	}
	return s[0]
}

// driverKeywordRanges walks dsn token by token the way pgconn's
// parseKeywordValueSettings does, so the keys it sees are the keys the driver
// sees: a key is the whitespace-trimmed run before '=', and may hold no
// whitespace — pgconn stops there, reporting `missing "=" after ...`, and
// reads nothing further. The value is read bare to the ASCII whitespace class
// or quoted, and the walk resumes AT THE VALUE'S END: after a quoted value no
// separator is needed, so in `host='db'password=s3cr3t` the next key is
// `password` — a scan that instead restarts after the previous '=' sees the
// key `'db'password` and hides nothing (#486). When the walk meets anything
// pgconn rejects — trailing bytes without an '=', a key holding whitespace, a
// quote never closed — the driver discards every setting it read, so no range
// the walk found is spelled beyond its own bytes: the rejection is tracked and
// the ranges come out fail-closed.
func driverKeywordRanges(dsn string) []secretRange {
	if strings.IndexByte(dsn, 0) >= 0 {
		// pgconn rejects a NUL outright; nothing is read from the string.
		return nil
	}
	var out []secretRange
	at := 0
	rejected := false
	for at < len(dsn) {
		for at < len(dsn) && isASCIISpace(dsn[at]) {
			at++
		}
		if at == len(dsn) {
			break
		}
		offset := strings.IndexByte(dsn[at:], '=')
		if offset < 0 {
			// pgconn: "invalid keyword/value" — the whole parse fails.
			rejected = true
			break
		}
		equals := at + offset
		key := strings.Trim(dsn[at:equals], asciiSpace)
		if i := strings.IndexAny(key, asciiSpace); i >= 0 {
			// pgconn: `missing "=" after ...` — the whole parse fails.
			rejected = true
			break
		}
		start, end, closed, ok := keywordValue(dsn, equals+1)
		if ok && isSecretConnKey(key) {
			out = append(out, secretRange{start: start, end: end, grammar: keywordGrammar, driver: isDriverConnKey(key)})
		}
		if !ok {
			// pgconn stores an empty value for a missing one and the loop
			// ends cleanly: nothing follows the trimmed spaces.
			break
		}
		if !closed {
			// A quote that is never closed: pgconn rejects the string.
			rejected = true
			break
		}
		at = end
	}
	if rejected {
		for i := range out {
			out[i].driver = false
		}
	}
	return out
}

// tolerantKeywordRanges adds the password-like values a stricter reading
// misses. A key is the run of non-whitespace bytes that ends just before an
// '=': `app:pass=word` reads the key `app:pass`, which no driver calls a
// password, while `host db password=x` still reaches `password`, because a
// reader that tolerates the typo gets there and a value is worth hiding before
// the grammar is settled. Whitespace may surround the '='. The value then reads
// the way pgconn reads one: quoted with ' it may hold a space, bare it ends only
// at the ASCII whitespace class, so '&', '"' and '?' are data, and in both a
// backslash escapes the next byte whatever it is -- newline included -- and one
// at the very end escapes the end of the value (#486).
func tolerantKeywordRanges(dsn string) []secretRange {
	var out []secretRange
	for at := 0; at < len(dsn); {
		offset := strings.IndexByte(dsn[at:], '=')
		if offset < 0 {
			break
		}
		equals := at + offset
		// pgconn reads the keyword as the run of bytes before '=' with
		// whitespace trimmed from both ends, and rejects a run that still holds
		// whitespace. A tolerant reading takes its last run: that is the key the
		// driver would have named had the token been spelled one place earlier,
		// and `password = s3cr3t` reaches it that way.
		key := strings.Trim(dsn[at:equals], asciiSpace)
		if i := strings.LastIndexAny(key, asciiSpace); i >= 0 {
			key = key[i+1:]
		}
		if start, end, _, ok := keywordValue(dsn, equals+1); ok && isSecretConnKey(key) {
			// The strict walk already marks the keys a driver reads; anything
			// only this reading finds is one the driver declined — a look-alike
			// key, or a string pgconn rejected before it got here — so its
			// value is hidden as written and spelled no further.
			out = append(out, secretRange{start: start, end: end, grammar: keywordGrammar, driver: false})
		}
		at = equals + 1
	}
	return out
}

// keywordValue reads the value of a keyword from the byte after its '=' the way
// pgconn's parseKeywordValueSettings does, and returns its range in dsn, with
// closed reporting whether a quoted value met its closing quote. An empty value
// has no range to hide, and a quote that is never closed hides the rest of the
// string: pgconn rejects that input, so nothing is printed from it.
func keywordValue(dsn string, from int) (start, end int, closed, ok bool) {
	i := from
	for i < len(dsn) && isASCIISpace(dsn[i]) {
		i++
	}
	if i == len(dsn) {
		return 0, 0, false, false
	}
	start = i
	if dsn[i] != '\'' {
		for i < len(dsn) {
			if isASCIISpace(dsn[i]) {
				break
			}
			if dsn[i] == '\\' {
				i++
				if i == len(dsn) {
					break
				}
			}
			i++
		}
		return start, i, true, i > start
	}
	i++
	for i < len(dsn) {
		if dsn[i] == '\'' {
			return start, i + 1, true, true
		}
		if dsn[i] == '\\' {
			i++
			if i == len(dsn) {
				break
			}
		}
		i++
	}
	return start, len(dsn), false, true
}

// mysqlPassword returns the range of the password go-sql-driver/mysql reads
// out of dsn, and only when the driver accepts the string at all: mysql
// rejects `postgres://app:pw@h:5432/db` (a bare `h:5432` is no network), and
// cutting the bytes its index rule would name on a string it refuses prints
// less than the operator asked to see. ParseDSN is a pure parse — no
// filesystem, no network — so asking it directly is the exact reading;
// pgconn.ParseConfig, by contrast, opens files a passfile or ssl setting
// names, which is why its rules are mirrored by hand instead. The parser
// reports no position, so the range is re-derived by the index rule mysql
// itself uses: the last '/' separates the database name, the last '@' before
// it ends the userinfo, and the password is what follows the first ':' before
// that — a password holding an '@', a '=' or a '/' is one field, not a prefix
// ending at the first separator (#486).
func mysqlPassword(dsn string) (secretRange, bool) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || cfg.Passwd == "" {
		return secretRange{}, false
	}
	slash := strings.LastIndexByte(dsn, '/')
	at := strings.LastIndexByte(dsn[:slash], '@')
	colon := strings.IndexByte(dsn[:at], ':')
	if slash <= 0 || at <= 0 || colon < 0 {
		// ParseDSN read a password, so its userinfo had a ':'; these guards
		// only keep the slicing honest.
		return secretRange{}, false
	}
	return secretRange{start: colon + 1, end: at, grammar: mysqlGrammar, driver: true}, true
}

// uriRanges returns the range of the password inside a connection URI's
// userinfo. `postgres://` and `postgresql://` — those spellings exactly — are
// read by pgconn's own libpq-matching parser, not by net/url: the userinfo is
// everything before the FIRST '@' provided no '/' comes before it, so '?' and
// '#' do not end it, and the password is what follows the first ':'. net/url
// ends the authority at '?' and '#', so `postgres://app:?pw@h/db` has no
// userinfo for it at all, and following it here would print the password
// whole (#486). Every other scheme keeps the net/url reading — the authority
// ends at '/', '?' or '#', the userinfo is what precedes its last '@', and the
// password follows the first ':' — which is what an Atlas-style `mysql://`
// or `sqlite://` URL carries.
func uriRanges(dsn string) []secretRange {
	rest, isPostgres, ok := cutScheme(dsn)
	if !ok {
		return nil
	}
	base := len(dsn) - len(rest)
	if isPostgres {
		if i := strings.IndexAny(rest, "@/"); i >= 0 && rest[i] == '@' {
			if colon := strings.IndexByte(rest[:i], ':'); colon >= 0 && colon+1 < i {
				return []secretRange{{start: base + colon + 1, end: base + i, grammar: uriGrammar, driver: true}}
			}
		}
		return nil
	}
	authority := rest
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		authority = rest[:end]
	}
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return nil
	}
	if colon := strings.IndexByte(authority[:at], ':'); colon >= 0 && colon+1 < at {
		return []secretRange{{start: base + colon + 1, end: base + at, grammar: uriGrammar, driver: true}}
	}
	return nil
}

// cutScheme splits a `scheme://` prefix off dsn and reports whether the scheme
// is one pgconn reads. A scheme is a letter followed by letters, digits, '+',
// '-' or '.', as url.Parse requires; a string whose first ':' is not followed
// by '//' — a MySQL DSN whose password holds one, say — has no authority to
// read (#486).
func cutScheme(dsn string) (rest string, isPostgres, ok bool) {
	colon := strings.IndexByte(dsn, ':')
	if colon < 1 || colon+2 >= len(dsn) || dsn[colon+1] != '/' || dsn[colon+2] != '/' {
		return dsn, false, false
	}
	scheme := dsn[:colon]
	if c := scheme[0]; (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
		return dsn, false, false
	}
	for i := 1; i < len(scheme); i++ {
		c := scheme[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '+', c == '-', c == '.':
		default:
			return dsn, false, false
		}
	}
	return dsn[colon+3:],
		scheme == "postgres" || scheme == "postgresql",
		true
}

// paramListStart returns the index of the '?' that opens a '&' separated
// parameter list, or -1 when the dsn has none. go-sql-driver/mysql reads its
// parameters from the first '?' after the last '/', and a SQLite database opened
// through a `file:` URI from its first '?'; the two are the only grammars here
// that separate parameters with '&' at all. pgconn's keyword grammar has no such
// separator, which is why this only ever adds a range to what the keyword
// readings have already taken: cutting the string here and reading the keyword
// grammar from the head alone printed the tail of `password=Sup3r?Secret?2026` (#486).
func paramListStart(dsn string) int {
	if isFileURI(dsn) {
		return strings.IndexByte(dsn, '?')
	}
	slash := strings.LastIndexByte(dsn, '/')
	if slash < 0 {
		return -1
	}
	if query := strings.IndexByte(dsn[slash+1:], '?'); query >= 0 {
		return slash + 1 + query
	}
	return -1
}

// isFileURI reports a SQLite URI filename, whose scheme is compared without
// regard to case even though no driver reads a password from it.
func isFileURI(dsn string) bool {
	return len(dsn) >= len("file:") && strings.EqualFold(dsn[:len("file:")], "file:")
}

// paramRanges returns the range of every password-like parameter's value in the
// '&' separated list that begins at offset. A key is the bytes before the first
// '=' of a parameter, and the grammar follows the string's: pgconn decodes a
// key out of a connection URI before it compares it, so an encoded spelling
// such as `pass%77ord` or a raw space such as `password ` names the password the
// driver stores, while go-sql-driver/mysql takes its parameters raw and reads no
// password out of any of them, and a SQLite URI decodes them its own way; the
// value is everything to the next '&', whatever bytes it holds.
func paramRanges(dsn string, offset int) []secretRange {
	g := uriGrammar
	driver := true
	if !isFileURI(dsn) && !isPostgresScheme(dsn) {
		g = mysqlGrammar
		driver = false
	}
	var out []secretRange
	at := offset
	for _, parameter := range strings.Split(dsn[offset:], "&") {
		key, _, hasValue := strings.Cut(parameter, "=")
		if start := at + len(key) + 1; hasValue && start < at+len(parameter) && isSecretConnKey(key) {
			read := driver
			if g == uriGrammar && !isFileURI(dsn) {
				// pgconn compares a URI key only after decoding it, and reads
				// two of them as secrets; the spellings follow that key.
				read = isDriverSecretKey(key)
			}
			out = append(out, secretRange{start: start, end: at + len(parameter), grammar: g, driver: read})
		}
		at += len(parameter) + 1
	}
	return out
}

func isPostgresScheme(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// isDriverConnKey reports whether a keyword/value key is one a driver itself
// reads a password under. pgconn reads `password` and `sslpassword` and
// nothing else — the comparison is case-sensitive, so `PASSWORD=` is a runtime
// parameter to it — while `pwd=`, `passwd=` and `pass=` are spellings a person
// writes for the same value, hidden fail-closed but spelled no further.
func isDriverConnKey(key string) bool {
	return key == "password" || key == "sslpassword"
}

// isDriverSecretKey reports whether a URI parameter key is one pgconn reads a
// password under: the key is compared only after it is decoded, so `pass%77ord`
// and `password ` name the password while `PASSWORD` does not. Decoding is
// pgURIDecode's, because that is the decoder that reads the key the driver reads.
func isDriverSecretKey(key string) bool {
	decoded, ok := pgURIDecode(key)
	return ok && isDriverConnKey(decoded)
}

// isSecretConnKey reports whether a parameter key names a password. It is decoded
// and compared without regard to case: pgconn decodes it but does not lower it,
// so both departures hide more than the driver would have read. The decode is
// pgURIDecode's where it succeeds — a key with the spaces pgconn drops is a
// password key to pgconn — and url.PathUnescape's otherwise, so a component
// pgconn refuses still hides its value fail-closed.
func isSecretConnKey(key string) bool {
	if decoded, ok := pgURIDecode(key); ok {
		key = decoded
	} else if decoded, err := url.PathUnescape(key); err == nil {
		key = decoded
	}
	_, ok := secretConnKeys[strings.ToLower(key)]
	return ok
}

func isASCIISpace(b byte) bool {
	return strings.IndexByte(asciiSpace, b) >= 0
}

func mergeRanges(ranges [][2]int) [][2]int {
	if len(ranges) == 0 {
		return nil
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i][0] != ranges[j][0] {
			return ranges[i][0] < ranges[j][0]
		}
		return ranges[i][1] < ranges[j][1]
	})
	merged := [][2]int{ranges[0]}
	for _, r := range ranges[1:] {
		last := &merged[len(merged)-1]
		if r[0] <= last[1] {
			if r[1] > last[1] {
				last[1] = r[1]
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// cutRanges replaces every range with one placeholder.
func cutRanges(dsn string, ranges [][2]int) string {
	if len(ranges) == 0 {
		return dsn
	}
	var b strings.Builder
	b.Grow(len(dsn) + len(ranges)*len(RedactedSecret))
	previous := 0
	for _, r := range ranges {
		b.WriteString(dsn[previous:r[0]])
		b.WriteString(RedactedSecret)
		previous = r[1]
	}
	b.WriteString(dsn[previous:])
	return b.String()
}

// SanitizeError returns err.Error() with known secrets from cfg removed.
func SanitizeError(err error, cfg Config) string {
	if err == nil {
		return ""
	}
	return SanitizeSecretText(err.Error(), cfg)
}

// SanitizeSecretText replaces known secrets from cfg inside text.
func SanitizeSecretText(text string, cfg Config) string {
	if text == "" {
		return text
	}
	if dsn := strings.TrimSpace(cfg.Database.DSN); dsn != "" {
		text = strings.ReplaceAll(text, dsn, RedactDSN(dsn))
		for _, secret := range dsnSecretValues(dsn) {
			text = strings.ReplaceAll(text, secret, RedactedSecret)
		}
	}
	if password := cfg.Cache.Redis.Password; password != "" {
		text = strings.ReplaceAll(text, password, RedactedSecret)
	}
	for _, secret := range []string{cfg.Auth.JWTSecret, cfg.Storage.URLSecret, cfg.Storage.S3.SecretAccessKey} {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, RedactedSecret)
		}
	}
	return text
}

// dsnSecretValues returns the passwords a dsn carries, in every spelling a
// message can hold them in: the bytes the dsn itself writes, plus — per the
// grammar that reads each one — the value a quote or a backslash escape stands
// for in a keyword/value string, or the value a percent escape stands for in a
// URI.
func dsnSecretValues(dsn string) []string {
	var out []string
	seen := make(map[string]struct{})
	add := func(value string) {
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	for _, r := range secretRanges(dsn) {
		if !r.driver {
			// A value no driver's parser stores as a password: it is hidden in
			// the DSN itself, and nothing of it reaches a message, because the
			// parser keeps no password to send — replacing it would rewrite text
			// that holds no secret.
			continue
		}
		value := dsn[r.start:r.end]
		add(value)
		switch r.grammar {
		case keywordGrammar:
			// A quoted value reaches the driver without its quotes, and a
			// backslash escapes the next byte whatever it is: `'it\'s'` is the
			// password it's.
			stored := unquoteDSNValue(value)
			add(stored)
			add(unescapeDSNValue(stored))
		case uriGrammar:
			// A decoded spelling shorter than four bytes is dropped: that is the
			// length at which a value like `%20` becomes a space, and replacing
			// a space rewrites the whole sentence rather than the secret it
			// stands for. The value as the dsn writes it is always replaced,
			// however short, because that is what the dsn itself prints.
			//
			// The decoding is pgURIDecode's, so the spelling is by construction
			// what pgconn stores: `postgres://app:s3cr3t @h/db` is the password
			// `s3cr3t`, not `s3cr3t `, and a component pgconn refuses stores
			// nothing, so nothing is spelled from it.
			if spelling, ok := pgURIDecode(value); ok && len(spelling) >= minDecodedSecretLen {
				add(spelling)
			}
		case mysqlGrammar:
			// go-sql-driver/mysql reads its userinfo by index and applies no
			// quoting, escaping or percent decoding: the bytes in the dsn are
			// the bytes on the wire, and no other spelling exists.
		}
	}
	// A decoded spelling is a substring of the one it was decoded from, so
	// replacing the longer first keeps the shorter one's own occurrences intact.
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// minDecodedSecretLen is how long a derived spelling must be before replacing it
// is safer than leaving it out.
const minDecodedSecretLen = 4

// unquoteDSNValue takes a libpq keyword value out of its quotes.
func unquoteDSNValue(value string) string {
	if len(value) >= 2 {
		if (value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

// unescapeDSNValue applies the backslash rule both libpq and pgconn use when
// they read a keyword value: a backslash is dropped and the character after it
// is taken literally, so `'it\'s'` is the password it's.
func unescapeDSNValue(value string) string {
	if !strings.ContainsRune(value, '\\') {
		return value
	}
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' {
			i++
			if i == len(value) {
				break
			}
		}
		b.WriteByte(value[i])
	}
	return b.String()
}
