package config

import (
	"net/url"
	"sort"
	"strings"
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
	if u, ok := parseConnURL(trimmed); ok {
		return redactURL(u)
	}
	return cutRanges(trimmed, passwordRanges(trimmed))
}

// parseConnURL reports the parsed URL when dsn is written as one that carries an
// authority. A MySQL net DSN holds "://" too whenever its password does
// (`root:p@ss://w0rd@tcp(db.internal:3306)/db`), and url.Parse reads that as an
// opaque URL whose scheme is the user name and whose userinfo, host and path are
// all empty: taking that branch would print neither the password nor the address
// the operator asked to see. go-sql-driver/mysql ignores "://" entirely, and
// pgconn only reads a URL when the string names a postgres scheme.
func parseConnURL(dsn string) (*url.URL, bool) {
	if !strings.Contains(dsn, "://") {
		return nil, false
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Opaque != "" {
		return nil, false
	}
	return u, u.Host != "" || u.User != nil
}

// passwordRanges returns the byte ranges of dsn that a driver reads as a
// password, with the overlapping ones merged into the placeholders they become.
func passwordRanges(dsn string) [][2]int {
	return mergeRanges(secretRanges(dsn))
}

func secretRanges(dsn string) [][2]int {
	ranges := keywordRanges(dsn)
	if start, end, ok := mysqlPassword(dsn); ok {
		ranges = append(ranges, [2]int{start, end})
	}
	if query := paramListStart(dsn); query >= 0 {
		ranges = append(ranges, paramRanges(dsn[query+1:], query+1)...)
	}
	return ranges
}

// keywordRanges returns the range of every password-like value a keyword/value
// reader takes out of dsn. A key is the run of non-whitespace bytes that ends
// just before an '=': `app:pass=word` reads the key `app:pass`, which no driver
// calls a password, while `host db password=x` still reaches `password`, because
// a reader that tolerates the typo gets there and a value is worth hiding before
// the grammar is settled. Whitespace may surround the '='. The value then reads
// the way pgconn reads one: quoted with ' it may hold a space, bare it ends only
// at the ASCII whitespace class, so '&', '"' and '?' are data, and in both a
// backslash escapes the next byte whatever it is -- newline included -- and one
// at the very end escapes the end of the value (#486).
func keywordRanges(dsn string) [][2]int {
	var out [][2]int
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
		if start, end, ok := keywordValue(dsn, equals+1); ok && isSecretConnKey(key) {
			out = append(out, [2]int{start, end})
		}
		at = equals + 1
	}
	return out
}

// keywordValue reads the value of a keyword from the byte after its '=' the way
// pgconn's parseKeywordValueSettings does, and returns its range in dsn. An
// empty value has no range to hide, and a quote that is never closed hides the
// rest of the string: pgconn rejects that input, so nothing is printed from it.
func keywordValue(dsn string, from int) (start, end int, ok bool) {
	i := from
	for i < len(dsn) && isASCIISpace(dsn[i]) {
		i++
	}
	if i == len(dsn) {
		return 0, 0, false
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
		return start, i, i > start
	}
	i++
	for i < len(dsn) {
		if dsn[i] == '\'' {
			return start, i + 1, true
		}
		if dsn[i] == '\\' {
			i++
			if i == len(dsn) {
				break
			}
		}
		i++
	}
	return start, len(dsn), true
}

// mysqlPassword returns the range of the password inside a go-sql-driver/mysql
// DSN, which reads its userinfo by index rather than by escaping or quoting:
// the last '/' separates the database name, the last '@' before it separates
// the userinfo, and the password is what follows the first ':' before that. A
// password holding an '@', a '=' or a '/' is therefore one field, not a prefix
// ending at the first separator (#486).
//
// A DSN without a '/' is not a net form at all: the driver reports
// "missing the slash separating the database name" before it reads a userinfo,
// so a keyword/value string whose password holds an '@' has no userinfo to cut.
func mysqlPassword(dsn string) (start, end int, ok bool) {
	slash := strings.LastIndexByte(dsn, '/')
	if slash <= 0 {
		return 0, 0, false
	}
	at := strings.LastIndexByte(dsn[:slash], '@')
	if at <= 0 {
		return 0, 0, false
	}
	colon := strings.IndexByte(dsn[:at], ':')
	if colon < 0 {
		return 0, 0, false
	}
	return colon + 1, at, true
}

// paramListStart returns the index of the '?' that opens a '&' separated
// parameter list, or -1 when the dsn has none. go-sql-driver/mysql reads its
// parameters from the first '?' after the last '/', and a SQLite database opened
// through a `file:` URI from its first '?'; the two are the only grammars here
// that separate parameters with '&' at all. pgconn's keyword grammar has no such
// separator, which is why this only ever adds a range to what keywordRanges has
// already taken: cutting the string here and reading the keyword grammar from
// the head alone printed the tail of `password=Sup3r?Secret?2026` (#486).
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
// '=' of a parameter, and pgconn decodes a key out of a connection URI before it
// compares it, so an encoded spelling such as `pass%77ord` names the password
// that goes on the wire; the value is everything to the next '&', whatever bytes
// it holds.
func paramRanges(list string, offset int) [][2]int {
	var out [][2]int
	at := offset
	for _, parameter := range strings.Split(list, "&") {
		key, _, hasValue := strings.Cut(parameter, "=")
		if start := at + len(key) + 1; hasValue && start < at+len(parameter) && isSecretConnKey(key) {
			out = append(out, [2]int{start, at + len(parameter)})
		}
		at += len(parameter) + 1
	}
	return out
}

// isSecretConnKey reports whether a parameter key names a password. It is decoded
// and compared without regard to case: pgconn decodes it but does not lower it,
// so both departures hide more than the driver would have read.
func isSecretConnKey(key string) bool {
	if decoded, err := url.PathUnescape(key); err == nil {
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

// rangeTexts returns the text of every range of s.
func rangeTexts(s string, ranges [][2]int) []string {
	out := make([]string, 0, len(ranges))
	for _, r := range ranges {
		out = append(out, s[r[0]:r[1]])
	}
	return out
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
// message can hold them in: the bytes the dsn itself writes, the value a
// backslash escape stands for, the value a percent escape stands for where a
// grammar decodes one, and a quoted value with or without its quotes.
func dsnSecretValues(dsn string) []string {
	if u, ok := parseConnURL(dsn); ok {
		var values []string
		if password, hasPassword := u.User.Password(); hasPassword {
			values = append(values, password)
			// pgconn ends a URL's userinfo at the first '@' where the parser it
			// is built on ends it at the last, so an unencoded '@' in a password
			// strands everything after it in the host: the bytes before the
			// strand are the password that driver puts on the wire.
			if at := strings.IndexByte(password, '@'); at > 0 {
				values = append(values, password[:at])
			}
		}
		if userinfo := u.User.String(); strings.Contains(userinfo, ":") {
			values = append(values, userinfo[strings.IndexByte(userinfo, ':')+1:])
		}
		values = append(values, rangeTexts(u.RawQuery, paramRanges(u.RawQuery, 0))...)
		return addValueVariants(values, percentDecoded)
	}

	var literal, encoded []string
	for _, r := range keywordRanges(dsn) {
		literal = append(literal, dsn[r[0]:r[1]])
	}
	if start, end, ok := mysqlPassword(dsn); ok {
		literal = append(literal, dsn[start:end])
	}
	if query := paramListStart(dsn); query >= 0 {
		// A driver that reads parameters this way decodes the value it finds,
		// and pgconn reads a URL parameter even when the string never reached
		// its URL branch.
		encoded = rangeTexts(dsn[query+1:], paramRanges(dsn[query+1:], 0))
	}
	return append(addValueVariants(literal, backslashOnly),
		addValueVariants(encoded, percentDecoded)...)
}

// spellings says how far a value's spellings are derived from the bytes a dsn
// writes. A keyword/value string and a MySQL net DSN read a value by unescaping
// backslashes alone; a connection URI's userinfo and parameters are percent
// decoded as well, and that decoded text is what the driver sends.
type spellings int

const (
	backslashOnly spellings = iota
	percentDecoded
)

// addValueVariants returns every spelling each value can reach a message in.
// A percent-decoded spelling shorter than four bytes is dropped: that is the one
// length at which a value like `%20` becomes a space, and replacing a space
// rewrites the whole sentence rather than the secret it stands for. The value as
// the dsn writes it is always replaced, however short, because that is what the
// dsn itself prints.
func addValueVariants(values []string, decoded spellings) []string {
	seen := make(map[string]struct{}, len(values)*2)
	out := make([]string, 0, len(values)*2)
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
	for _, value := range values {
		add(value)
		stored := unquoteDSNValue(value)
		add(stored)
		add(unescapeDSNValue(stored))
		if decoded != percentDecoded {
			continue
		}
		// pgconn reads a URL parameter with uriDecode, which leaves '+' alone,
		// so this is PathUnescape rather than QueryUnescape.
		if unescaped := unescapeDSNValue(stored); unescaped != "" {
			if spelling, err := url.PathUnescape(unescaped); err == nil && len(spelling) >= minDecodedSecretLen {
				add(spelling)
				add(unescapeDSNValue(spelling))
			}
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

func redactURL(u *url.URL) string {
	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString("://")
	if u.User != nil {
		b.WriteString(u.User.Username())
		if _, hasPassword := u.User.Password(); hasPassword {
			b.WriteByte(':')
			b.WriteString(RedactedSecret)
		}
		b.WriteByte('@')
	}
	b.WriteString(u.Host)
	if path := u.EscapedPath(); path != "" {
		b.WriteString(path)
	}
	if u.RawQuery != "" {
		b.WriteByte('?')
		b.WriteString(cutRanges(u.RawQuery, mergeRanges(paramRanges(u.RawQuery, 0))))
	}
	if u.Fragment != "" {
		b.WriteByte('#')
		b.WriteString(u.Fragment)
	}
	return b.String()
}
