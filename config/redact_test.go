package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRedactDSNHidesURLPassword(t *testing.T) {
	t.Parallel()

	dsn := "postgres://gombit:super-secret@127.0.0.1:5432/gombit?sslmode=disable" // #nosec G101 -- fake local test DSN.
	got := RedactDSN(dsn)
	if strings.Contains(got, "super-secret") {
		t.Fatalf("RedactDSN() = %q, still contains password", got)
	}
	if !strings.Contains(got, RedactedSecret) {
		t.Fatalf("RedactDSN() = %q, want %s placeholder", got, RedactedSecret)
	}
	if !strings.Contains(got, "gombit") || !strings.Contains(got, "127.0.0.1") {
		t.Fatalf("RedactDSN() = %q, want username and host preserved", got)
	}
}

func TestRedactDSNHidesMySQLUserinfo(t *testing.T) {
	t.Parallel()

	dsn := "gombit:mysql-secret@tcp(127.0.0.1:3306)/gombit?parseTime=true"
	got := RedactDSN(dsn)
	if strings.Contains(got, "mysql-secret") {
		t.Fatalf("RedactDSN() = %q, still contains password", got)
	}
	if got != "gombit:"+RedactedSecret+"@tcp(127.0.0.1:3306)/gombit?parseTime=true" {
		t.Fatalf("RedactDSN() = %q, want mysql userinfo redacted", got)
	}
}

func TestRedactDSNLeavesSQLiteFilePath(t *testing.T) {
	t.Parallel()

	dsn := "file:gombit.db?cache=shared&_fk=1"
	if got := RedactDSN(dsn); got != dsn {
		t.Fatalf("RedactDSN() = %q, want unchanged sqlite DSN", got)
	}
}

func TestRedactDSNHidesQueryPassword(t *testing.T) {
	t.Parallel()

	dsn := "file:gombit.db?cache=shared&password=query-secret"
	got := RedactDSN(dsn)
	if strings.Contains(got, "query-secret") {
		t.Fatalf("RedactDSN() = %q, still contains query password", got)
	}
	if !strings.Contains(got, "password="+RedactedSecret) {
		t.Fatalf("RedactDSN() = %q, want password query redacted", got)
	}
}

func TestRedactDSNHidesLibpqKeywordPassword(t *testing.T) {
	t.Parallel()

	dsn := "host=localhost user=gombit password=s3cret dbname=app sslmode=disable" // #nosec G101 -- fake local test DSN.
	got := RedactDSN(dsn)
	if strings.Contains(got, "s3cret") {
		t.Fatalf("RedactDSN() = %q, still contains password", got)
	}
	want := "host=localhost user=gombit password=" + RedactedSecret + " dbname=app sslmode=disable"
	if got != want {
		t.Fatalf("RedactDSN() = %q, want %q", got, want)
	}
}

func TestRedactDSNHidesQuotedLibpqPassword(t *testing.T) {
	t.Parallel()

	dsn := `host=localhost password='has spaces' dbname=app` // #nosec G101 -- fake local test DSN.
	got := RedactDSN(dsn)
	if strings.Contains(got, "has spaces") {
		t.Fatalf("RedactDSN() = %q, still contains password", got)
	}
	want := "host=localhost password=" + RedactedSecret + " dbname=app"
	if got != want {
		t.Fatalf("RedactDSN() = %q, want %q", got, want)
	}
}

// The drivers' own readings of a DSN are the contract: whatever password
// go-sql-driver/mysql or pgconn would send is what a printed DSN must not
// contain. mysql splits userinfo at the last '@' before the last '/' and reads
// no escaping at all, while libpq — whose parser pgconn mirrors — allows
// whitespace around '=', ends an unquoted value only at whitespace (so '&', '"'
// and '?' are ordinary data), and escapes with backslashes. A first '@' split
// and a strict `key=` match leave part of the password in the open (#486).
func TestRedactDSNHidesPasswordsTheDriversAccept(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, dsn, want string }{
		{
			name: "mysql password with an at sign",
			dsn:  "app:p@ssw0rd-tail@tcp(db.internal:3306)/app?parseTime=true",
			want: "app:" + RedactedSecret + "@tcp(db.internal:3306)/app?parseTime=true",
		},
		{
			name: "mysql at sign in a query parameter",
			dsn:  "app:secret@tcp(db.internal:3306)/app?name=a@b",
			want: "app:" + RedactedSecret + "@tcp(db.internal:3306)/app?name=a@b",
		},
		{
			name: "mysql password with a slash and an at sign",
			dsn:  "app:p/sa@ss@tcp(db.internal:3306)/app",
			want: "app:" + RedactedSecret + "@tcp(db.internal:3306)/app",
		},
		{
			name: "mysql password with an equals sign",
			dsn:  "app:pa=ss@tcp(db.internal:3306)/app",
			want: "app:" + RedactedSecret + "@tcp(db.internal:3306)/app",
		},
		{
			name: "libpq whitespace around the equals sign",
			dsn:  "host=db user=app password = s3cr3t-value dbname=app",
			want: "host=db user=app password = " + RedactedSecret + " dbname=app",
		},
		{
			name: "libpq vertical tab around the equals sign",
			dsn:  "host=db user=app password\x0b=s3cr3t-value dbname=app",
			want: "host=db user=app password\x0b=" + RedactedSecret + " dbname=app",
		},
		{
			name: "libpq quoted password with an escaped quote",
			dsn:  `host=db user=app password='it\'s-a-s3cr3t tail' dbname=app`,
			want: "host=db user=app password=" + RedactedSecret + " dbname=app",
		},
		{
			name: "libpq unquoted password with an escaped space",
			dsn:  `host=db password=s3cr3t\ value dbname=app`,
			want: "host=db password=" + RedactedSecret + " dbname=app",
		},
		{
			name: "libpq password with an at sign and a port in the host",
			dsn:  "host=localhost:5432 user=app password=p@ss dbname=app",
			want: "host=localhost:5432 user=app password=" + RedactedSecret + " dbname=app",
		},
		{
			name: "libpq password with an ampersand",
			dsn:  "host=db user=app password=s3cr3t&more dbname=app",
			want: "host=db user=app password=" + RedactedSecret + " dbname=app",
		},
		{
			name: "libpq password in double quotes",
			dsn:  `host=db user=app password="s3cr3t"&more dbname=app`,
			want: "host=db user=app password=" + RedactedSecret + " dbname=app",
		},
		{
			name: "the pwd and pass look-alikes",
			dsn:  "host=db user=app pwd=s3cr3t pass=tail dbname=app",
			want: "host=db user=app pwd=" + RedactedSecret + " pass=" + RedactedSecret + " dbname=app",
		},
		{
			name: "an ampersand still ends a query parameter",
			dsn:  "file:gombit.db?cache=shared&password=query-secret&sslmode=disable",
			want: "file:gombit.db?cache=shared&password=" + RedactedSecret + "&sslmode=disable",
		},
		{
			// pgconn ends a value at whitespace, so a keyword list carries a '?' as
			// ordinary data: the parameter reading must not cut the password there.
			name: "a question mark inside a libpq password",
			dsn:  "host=db user=app password=pa/ss?word dbname=app",
			want: "host=db user=app password=" + RedactedSecret + " dbname=app",
		},
		{
			name: "a question mark inside a quoted libpq password",
			dsn:  "host=db user=app password='pa/ss?word' dbname=app",
			want: "host=db user=app password=" + RedactedSecret + " dbname=app",
		},
		{
			name: "a socket path and a question mark in a libpq password",
			dsn:  "host=/var/run/postgresql password=s3cr3t?x dbname=app",
			want: "host=/var/run/postgresql password=" + RedactedSecret + " dbname=app",
		},
		{
			name: "a libpq password ending the DSN",
			dsn:  "dbname=mydb password=pa/ss?word",
			want: "dbname=mydb password=" + RedactedSecret,
		},
		{
			name: "an equals sign in the user name of a mysql net dsn",
			dsn:  "a=b:p@ssw0rd@tcp(db.internal:3306)/app",
			want: "a=b:" + RedactedSecret + "@tcp(db.internal:3306)/app",
		},
		{
			name: "a space inside a mysql userinfo",
			dsn:  "app:my pass@tcp(db.internal:3306)/db",
			want: "app:" + RedactedSecret + "@tcp(db.internal:3306)/db",
		},
		{
			// A keyword reading that matched `pass=` wherever it appears would
			// take the address with it. pgconn reads the key of that token as
			// `app:pass`, which is not a password key, so only the MySQL
			// userinfo goes.
			name: "a password-like key inside a mysql userinfo",
			dsn:  "app:pass=word@tcp(db.internal:3306)/db",
			want: "app:" + RedactedSecret + "@tcp(db.internal:3306)/db",
		},
		{
			name: "a password-like key inside a mysql userinfo, spelled with the key's own name",
			dsn:  "app:password=word@tcp(db.internal:3306)/db",
			want: "app:" + RedactedSecret + "@tcp(db.internal:3306)/db",
		},
		{
			// An unencoded "://" in a MySQL password makes url.Parse read the
			// user name as a scheme and the whole DSN as an opaque URL, which
			// leaves no userinfo for the URL branch to mask and nothing to
			// print: go-sql-driver/mysql ignores "://" and wants its range.
			name: "a double slash inside a mysql password",
			dsn:  "app:x://y@tcp(db.internal:3306)/db",
			want: "app:" + RedactedSecret + "@tcp(db.internal:3306)/db",
		},
		{
			name: "an at sign and a double slash inside a mysql password",
			dsn:  "root:p@ss://w0rd@tcp(db.internal:3306)/db",
			want: "root:" + RedactedSecret + "@tcp(db.internal:3306)/db",
		},
		{
			// pgconn separates keyword tokens on whitespace and never cuts a
			// value at '?', so a parameter reading may add a range but may not
			// end the keyword reading where the '?' falls.
			name: "a question mark inside a libpq password of a socket dsn with an at sign",
			dsn:  "user=app@corp host=/tmp password=Sup3r?Secret?2026 dbname=app",
			want: "user=app@corp host=/tmp password=" + RedactedSecret + " dbname=app",
		},
		{
			// A connection URI's parameter key is decoded before it is compared,
			// because that is how pgconn decides what the parameter is.
			name: "a percent-encoded password key in a URL parameter",
			dsn:  "postgres://app@h:5432/db?pass%77ord=s3cr3t-tail&sslmode=disable",
			want: "postgres://app@h:5432/db?pass%77ord=" + RedactedSecret + "&sslmode=disable",
		},
		{
			name: "an sslpassword key of a libpq dsn",
			dsn:  "host=db user=app sslpassword=tls-s3cr3t dbname=app sslmode=disable",
			want: "host=db user=app sslpassword=" + RedactedSecret + " dbname=app sslmode=disable",
		},
		{
			name: "an sslpassword URL parameter",
			dsn:  "postgres://app@h:5432/db?sslpassword=tls-s3cr3t&sslmode=disable",
			want: "postgres://app@h:5432/db?sslpassword=" + RedactedSecret + "&sslmode=disable",
		},
		{
			// passfile names the file a password is read from, not a secret.
			name: "a passfile path",
			dsn:  "host=db user=app passfile=/home/deploy/.pgpass dbname=app",
			want: "host=db user=app passfile=/home/deploy/.pgpass dbname=app",
		},
		{
			// Whitespace around '=' is a separator to pgconn, and after the '='
			// it trims the leading whitespace off the value before it reads it:
			// an empty password is the next token, which is what the driver
			// sends.
			name: "a libpq password that is empty up to the next token",
			dsn:  "host=db user=app password= dbname=x sslmode=disable",
			want: "host=db user=app password= " + RedactedSecret + " sslmode=disable",
		},
		{
			name: "a password parameter of a mysql net dsn with no userinfo",
			dsn:  "tcp(db.internal:3306)/db?password=s3cr3t&parseTime=true",
			want: "tcp(db.internal:3306)/db?password=" + RedactedSecret + "&parseTime=true",
		},
		{
			// A URI scheme is compared without regard to case.
			name: "an upper-case file scheme with a password parameter",
			dsn:  "FILE:gombit.db?password=s3cr3t&x=1",
			want: "FILE:gombit.db?password=" + RedactedSecret + "&x=1",
		},
		{
			// pgconn reads a URL's userinfo up to the FIRST '@' — its own
			// libpq-matching parser, not net/url's last-'@' rule — so the
			// password it sends is `sup3r` and the `ss@h` bytes after it are
			// host data: only the driver's own reading is hidden. net/url
			// would name `sup3r@ss` the password, and hiding that ate the
			// host with it. go-sql-driver/mysql rejects the string outright
			// (a bare `h` names no network), so no second reading widens the
			// range back (#486).
			name: "an unencoded at sign in a URL password",
			dsn:  "postgres://app:sup3r@ss@h:5432/db?sslmode=disable",
			want: "postgres://app:" + RedactedSecret + "@ss@h:5432/db?sslmode=disable",
		},
		{
			name: "a password-like query parameter of a mysql net dsn",
			dsn:  "app:secret@tcp(db.internal:3306)/db?password=a&other=1",
			want: "app:" + RedactedSecret + "@tcp(db.internal:3306)/db?password=" + RedactedSecret + "&other=1",
		},
		{
			name: "a percent-escaped password in a URL parameter",
			dsn:  "postgres://app@h:5432/db?password=a%40b%20c&sslmode=disable",
			want: "postgres://app@h:5432/db?password=" + RedactedSecret + "&sslmode=disable",
		},
		{
			// net/url ends a URL authority at '?' and '#', so it sees no
			// userinfo here at all; pgconn's own parser reads the userinfo to
			// the first '@' whatever bytes it holds, and that password is what
			// it sends.
			name: "a question mark that opens nothing in a URL password",
			dsn:  "postgres://app:?s3cr3tvalue@h/db",
			want: "postgres://app:" + RedactedSecret + "@h/db",
		},
		{
			name: "a hash inside a URL password",
			dsn:  "postgres://app:#s3cr3tvalue@h/db",
			want: "postgres://app:" + RedactedSecret + "@h/db",
		},
		{
			name: "a port-shaped prefix inside a URL password",
			dsn:  "postgres://app:5432?s3cr3tvalue@h/db",
			want: "postgres://app:" + RedactedSecret + "@h/db",
		},
		{
			// A MySQL password beginning with '//' gives net/url a valid
			// authority and no password: the URI reading names nothing, and
			// the driver's index reading is what hides the field.
			name: "a mysql password that begins with a double slash, on a tcp address",
			dsn:  "app://s3cr3tvalue@tcp(db.internal)/db",
			want: "app:" + RedactedSecret + "@tcp(db.internal)/db",
		},
		{
			name: "a mysql password that begins with a double slash, on the default address",
			dsn:  "app://s3cr3tvalue@/db",
			want: "app:" + RedactedSecret + "@/db",
		},
		{
			name: "a mysql password that begins with a double slash, on a unix socket",
			dsn:  "app://s3cr3tvalue@unix(/tmp/mysql.sock)/db",
			want: "app:" + RedactedSecret + "@unix(/tmp/mysql.sock)/db",
		},
		{
			// pgconn resumes at a quoted value's end with no separator, so the
			// key after `'db'` is `password`; a scan that restarts after the
			// previous '=' sees the key `'db'password` and hides nothing.
			name: "a key placed directly after a closing quote",
			dsn:  "host='db'password=s3cr3tvalue dbname=app",
			want: "host='db'password=" + RedactedSecret + " dbname=app",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactDSN(tt.dsn); got != tt.want {
				t.Fatalf("RedactDSN(%q) = %q, want %q", tt.dsn, got, tt.want)
			}
		})
	}
}

// driverCheckedDSNs is the corpus both helpers are checked against: every string
// is one a driver reads a password out of, and every one of them came from the
// parsers' own rules rather than from the redaction's. An entry must parse
// without touching the filesystem — no sslrootcert, sslkey, passfile or service
// path — because pgconn opens what those name, and a corpus that passed on one
// machine and failed on the next would check nothing.
var driverCheckedDSNs = []string{
	"app:p@ssw0rd-tail@tcp(db.internal:3306)/app?parseTime=true",
	"app:p/sa@ss@tcp(db.internal:3306)/app",
	"app:pa=ss@tcp(db.internal:3306)/app",
	"app:secret@tcp(db.internal:3306)/app?parseTime=true&charset=utf8mb4",
	"app:pass=word@tcp(db.internal:3306)/db",
	"app:password=s3cr3t@tcp(db.internal:3306)/db",
	"app:my pass@tcp(db.internal:3306)/db",
	"app:x://y@tcp(db.internal:3306)/db",
	"root:p@ss://w0rd@tcp(db.internal:3306)/db",
	"a=b:p@ssw0rd@tcp(db.internal:3306)/app",
	"app:secret@tcp(db.internal:3306)/db?password=a&other=1",
	"host=db.internal user=app password = s3cr3t-value dbname=app",
	"host=db.internal user=app password\x0b=s3cr3t-value dbname=app",
	`host=db.internal user=app password='it\'s-a-s3cr3t tail' dbname=app`,
	`host=db.internal password=s3cr3t\ value dbname=app`,
	"host=localhost:5432 user=app password=p@ss dbname=app",
	"host=localhost:5432 password=s3cr3t@tcp(db.internal:3306)/db",
	"host=db.internal user=app password=s3cr3t&more dbname=app",
	`host=db.internal user=app password="s3cr3t"&more dbname=app`,
	"host=db user=app password=pa/ss?word dbname=app",
	"host=db user=app password='pa/ss?word' dbname=app",
	"user=app@corp host=/tmp password=Sup3r?Secret?2026 dbname=app",
	"host=db.internal user=app password= dbname=x sslmode=disable",
	"host=/var/run/postgresql password=s3cr3t?x dbname=app",
	"dbname=mydb password=pa/ss?word",
	"postgres://app:s3cr3t%40tail@db.internal:5432/app?sslmode=disable",
	"postgres://app:sup3r@ss@h/db?sslmode=disable",
	"postgres://app@h:5432/db?pass%77ord=s3cr3t-tail&sslmode=disable",
	"postgres://app@h:5432/db?password=a%40b%20c&sslmode=disable",
	// pgconn's URL parser ends the userinfo at the first '@' with no '/' before
	// it, so a '?' or a '#' inside the password is data to the driver where
	// net/url would end the authority at it.
	"postgres://app:?s3cr3tvalue@h/db",
	"postgres://app:#s3cr3tvalue@h/db",
	"postgres://app:5432?s3cr3tvalue@h/db",
	// A MySQL password starting with '//' gives net/url a valid authority and
	// no password at all; the driver reads it by index and hides nothing.
	"app://s3cr3tvalue@tcp(db.internal)/db",
	"app://s3cr3tvalue@/db",
	"app://s3cr3tvalue@unix(/tmp/mysql.sock)/db",
	// pgconn needs no separator after a quoted value, so the next key begins
	// right at the closing quote.
	"host='db'password=s3cr3tvalue dbname=app",
}

// Redaction is checked against the parsers that build the connection, not
// against a reading of the DSN by hand: whatever password the driver takes from
// a string it accepts must be gone from RedactDSN's output — its tail as well as
// its whole.
func TestRedactDSNHidesWhatTheDriversParse(t *testing.T) {
	t.Parallel()

	for _, dsn := range driverCheckedDSNs {
		dsn := dsn
		t.Run(dsn, func(t *testing.T) {
			t.Parallel()
			assertNoDriverPassword(t, dsn, "RedactDSN", RedactDSN(dsn))
		})
	}
}

// A message that quotes a DSN, or the password the server rejected out of it,
// is sanitised from the same readings: the escapes the DSN spells stand for the
// password the driver sends (#486).
func TestSanitizeErrorHidesWhatTheDriversParse(t *testing.T) {
	t.Parallel()

	for _, dsn := range driverCheckedDSNs {
		dsn := dsn
		t.Run(dsn, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Database.DSN = dsn // #nosec G101 -- fake local test DSN.
			for _, password := range mustDriverPasswords(t, dsn) {
				// The prose is kept free of the password's own letters: the
				// check below looks for fragments, and a word like "password"
				// would answer it on its own.
				err := errors.New("ping failed for " + dsn + " [" + password + "]")
				assertNoDriverPassword(t, dsn, "SanitizeError", SanitizeError(err, cfg))
			}
		})
	}
}

// assertNoDriverPassword fails unless every password a driver reads out of dsn
// is absent from printed. The tail is checked apart from the whole: a redaction
// that rewrites the head of a password and leaves the rest visible hides
// nothing that matters.
func assertNoDriverPassword(t *testing.T, dsn, helper string, printed string) {
	t.Helper()
	for _, password := range mustDriverPasswords(t, dsn) {
		assertPasswordHidden(t, dsn, helper, printed, password)
	}
}

// assertPasswordHidden fails unless password, and a long tail of it, are absent
// from printed. A password cut at the wrong separator leaves its end behind,
// and the shortest leak of that shape is the last three bytes: without this a
// four-character password passes on its whole alone. The check runs on each
// piece the placeholders split printed into: a password whose bytes the
// placeholder itself spells (`0***` against `*****`) is hidden by it, not
// leaked through it, and a fragment assembled from a placeholder's edge and
// the address around it is not something a reader can reassemble either.
func assertPasswordHidden(t *testing.T, dsn, helper, printed, password string) {
	t.Helper()
	fragments := []string{password}
	switch {
	case len(password) >= 8:
		fragments = append(fragments, password[len(password)/2:])
	case len(password) >= 4:
		fragments = append(fragments, password[len(password)-3:])
	}
	for _, fragment := range fragments {
		for _, piece := range strings.Split(printed, RedactedSecret) {
			if strings.Contains(piece, fragment) {
				t.Errorf("%s(%q) = %q, still contains %q of the driver's password %q", helper, dsn, printed, fragment, password)
				return
			}
		}
	}
}

// driverPasswords returns the passwords the database drivers take from dsn, one
// per driver that reads a password out of it. A DSN that no driver reads a
// password from is not a leak to check: `pwd=` is a runtime parameter to
// pgconn, not a password, and the MySQL driver wants a database name.
func driverPasswords(dsn string) []string {
	var out []string
	if cfg, err := mysql.ParseDSN(dsn); err == nil && cfg.Passwd != "" {
		out = append(out, cfg.Passwd)
	}
	if cfg, err := pgconn.ParseConfig(dsn); err == nil && cfg.Password != "" {
		out = append(out, cfg.Password)
	}
	return out
}

func mustDriverPasswords(t *testing.T, dsn string) []string {
	t.Helper()
	passwords := driverPasswords(dsn)
	if len(passwords) == 0 {
		t.Fatalf("no driver takes a password from %q, so this case checks nothing", dsn)
	}
	return passwords
}

func TestSanitizeErrorHidesAMySQLPasswordWithAnAtSign(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Database.DSN = "app:p@ssw0rd-tail@tcp(db.internal:3306)/app?parseTime=true" // #nosec G101 -- fake local test DSN.

	got := SanitizeError(errors.New("connect failed for dsn "+cfg.Database.DSN), cfg)
	want := "connect failed for dsn app:" + RedactedSecret + "@tcp(db.internal:3306)/app?parseTime=true"
	if got != want {
		t.Fatalf("SanitizeError() = %q, want %q", got, want)
	}
}

func TestSanitizeErrorHidesALibpqPasswordWithSpacesAroundEquals(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Database.DSN = "host=db user=app password = s3cr3t-value dbname=app" // #nosec G101 -- fake local test DSN.

	got := SanitizeError(errors.New(`startup packet rejected s3cr3t-value`), cfg)
	if strings.Contains(got, "s3cr3t") {
		t.Fatalf("SanitizeError() = %q, still contains the libpq password", got)
	}
	if !strings.Contains(got, RedactedSecret) {
		t.Fatalf("SanitizeError() = %q, want the password redacted", got)
	}
}

// pgconn puts a URL parameter's password on the wire percent-decoded, so a
// message naming what the server rejected names bytes the DSN never spells:
// both spellings have to go (#486).
func TestSanitizeErrorHidesTheDecodedPasswordOfAURLParameter(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Database.DSN = "postgres://app@h:5432/db?password=a%40b%20c&sslmode=disable" // #nosec G101 -- fake local test DSN.

	got := SanitizeError(errors.New("handshake failed for postgres://app@h:5432/db?password=a%40b%20c&sslmode=disable: password a@b c rejected"), cfg)
	for _, fragment := range []string{"a@b c", "a%40b%20c", "b c"} {
		if strings.Contains(got, fragment) {
			t.Fatalf("SanitizeError() = %q, still contains %q", got, fragment)
		}
	}
	if !strings.Contains(got, "sslmode=disable") {
		t.Fatalf("SanitizeError() = %q, want the rest of the message readable", got)
	}
}

// A backslash escapes the next byte whatever it is, and pgconn does not consult
// a character class before deciding which byte that is: an escaped newline stays
// inside the value, so a reading that stops at the newline leaves the rest of the
// password printed (#486).
func TestRedactDSNHidesABackslashEscapedNewline(t *testing.T) {
	t.Parallel()

	dsn := "host=db.internal password=abc\\\ndef=1 dbname=x sslmode=disable"
	if passwords := driverPasswords(dsn); len(passwords) != 1 || passwords[0] != "abc\ndef=1" {
		t.Fatalf("the drivers take %q as the password of %q, want it to hold the escaped newline", passwords, dsn)
	}
	got := RedactDSN(dsn)
	assertNoDriverPassword(t, dsn, "RedactDSN", got)
	if want := "host=db.internal password=" + RedactedSecret + " dbname=x sslmode=disable"; got != want {
		t.Fatalf("RedactDSN(%q) = %q, want %q", dsn, got, want)
	}
}

// Each password is replaced as a literal, so a spelling the driver never sends
// must not join the list: a keyword/value string is unescaped by backslash rules
// only, and decoding its `%20` would stand for a space — which every message
// holds, and which would then be deleted wherever it appears (#486).
func TestSanitizeErrorKeepsAKeywordPasswordSpellingThatOnlyAURLDecodes(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Database.DSN = `host=db user=app password=%20 dbname=x sslmode=disable` // #nosec G101 -- fake local test DSN.

	message := `pq: FATAL: password authentication failed for user "app" on db.internal:5432`
	if got := SanitizeError(errors.New(message), cfg); got != message {
		t.Fatalf("SanitizeError() = %q, want the message untouched", got)
	}
}

// Each grammar's spellings are its own: go-sql-driver/mysql applies no quoting
// or escaping, so unquoting its password `'p'` derived the spelling `p` and
// deleted every letter p of a message that never held the password, which is
// the symptom #486 opened with. A URI value is percent-decoded only — unquoting
// `'s'` there blanked every s — and a backslash inside a MySQL userinfo stays
// as written, or the spelling `app` deletes the user name from every message.
func TestSanitizeErrorLeavesTextThatHoldsNoSpellingOfThePasswordAlone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dsn  string
		text string
	}{
		{
			name: "a quoted one-letter mysql password is not unquoted",
			dsn:  "app:'p'@tcp(db.internal:3306)/app?parseTime=true",
			text: "ping failed: the app is unreachable on db.internal",
		},
		{
			name: "a quoted one-letter URL password is not unquoted",
			dsn:  "postgres://app:'s'@h:5432/db",
			text: "ping failed: the app is unreachable on the database host",
		},
		{
			name: "a backslash in a mysql userinfo is not unescaped",
			dsn:  `app:a\pp@tcp(db.internal:3306)/db`,
			text: "ping failed: the app is unreachable on db.internal",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Database.DSN = tt.dsn // #nosec G101 -- fake local test DSN.
			if got := SanitizeError(errors.New(tt.text), cfg); got != tt.text {
				t.Fatalf("SanitizeError() = %q, want the message untouched (it holds no spelling of the password)", got)
			}
		})
	}
}

// pgconn reads a connection URI's settings or none of them: a pair with a second
// raw '=' or no '=' at all, a bad percent escape, a decoded NUL, an interior space
// — anywhere in the URI, userinfo, host, port or path included — fails
// parseURLSettings and the settings map comes back empty. A password written in a
// URI the driver refuses therefore reaches no wire, so no spelling of it may be
// derived from it: the DSN still hides it fail-closed, and a message that happens
// to hold those bytes is left alone.
func TestSanitizeErrorSpellsNoPasswordFromAURITheDriverRefuses(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, dsn, text string }{
		{
			name: "a query pair with a second equals sign",
			dsn:  "postgres://h/db?password=status=failed",
			text: "connection status=failed",
		},
		{ // #nosec G101 -- fake local test DSNs.
			name: "a query pair with no equals sign",
			dsn:  "postgres://app:Sup3r@h/db?password=Secret2&broken",
			text: "login Secret2 as app with Sup3r failed",
		},
		{
			name: "a host with a bad percent escape",
			dsn:  "postgres://app:Sup3r@h%zz/db?password=Secret2",
			text: "password Secret2 rejected for host h%zz",
		},
		{ // #nosec G101 -- fake local test DSN.
			name: "a database name with an interior space",
			dsn:  "postgres://app:Sup3r@h/my db?password=Secret2",
			text: "Secret2 opened my db as app with Sup3r",
		},
		{
			name: "a query value decoding to a NUL",
			dsn:  "postgres://h/db?password=%00Sup3r",
			text: "login %00Sup3r failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Database.DSN = tt.dsn // #nosec G101 -- fake local test DSN.

			if got := SanitizeError(errors.New(tt.text), cfg); got != tt.text {
				t.Fatalf("SanitizeError() = %q, want the message untouched: pgconn reads no setting out of %q", got, tt.dsn)
			}
			// Hidden in the dsn itself, however: the bytes are still a password
			// as an operator wrote it, whatever the driver makes of the string.
			redacted := RedactDSN(tt.dsn)
			for _, secret := range []string{"Secret2", "Sup3r", "%00Sup3r"} {
				if strings.Contains(redacted, secret) {
					t.Fatalf("RedactDSN() = %q, still shows %q", redacted, secret)
				}
			}
		})
	}
}

// pgURICheckedDSNs are URIs from both sides of the line pgURIRejected draws:
// every one refused by pgconn's own parser for a reason the mirror claims to
// follow — a bad percent escape or an interior space in a component before the
// query, a pair with no '=' or with a second raw one, a NUL — and every one
// accepted with a secret in its userinfo or its query, so the check cannot pass
// by calling everything rejected.
var pgURICheckedDSNs = []string{
	"postgres://h/db?password=status=failed",
	"postgres://app:Sup3r@h/db?password=Secret2&broken",
	"postgres://app:Sup3r@h%zz/db?password=Secret2",
	"postgres://app:Sup3r@h/my db?password=Secret2",
	"postgres://h/db?password=%00Sup3r",
	"postgres://app:%zzSup3r@h/db?password=Secret2",
	"postgres://[::1/db?password=Secret2",
	"postgres://[]:5432/db?password=Secret2",
	"postgres://h:5%zz0/db?password=Secret2",
	"postgres://h/db?%zzpassword=Secret2",
	"postgres://h/db?password=Secret2\x00",
	"postgres://app:Sup3r@h:5432/db?password=Secret2&sslmode=disable",
	"postgres://app:Sup3r@[::1]:5432/db?sslmode=disable",
	"postgres://app:Sup3r@h1,h2:5432/db",
	"postgresql://app@h/db?password=Sup3r%40tail",
}

// The mirror is pinned to the parser it mirrors: pgURIRejected answers what it
// would have to answer for the redaction to derive a password's spellings from a
// URI — the driver read a setting out of it, or it read none.
func TestPgURIRejectedAgreesWithTheDriver(t *testing.T) {
	t.Parallel()

	for _, dsn := range pgURICheckedDSNs {
		dsn := dsn
		t.Run(dsn, func(t *testing.T) {
			t.Parallel()
			_, err := pgconn.ParseConfig(dsn) // #nosec G101 -- fake local test DSN.
			// Only the URI's own grammar is in question: a config that fails
			// for anything later is one pgconn read settings out of, and the
			// corpus names no file a later step would open.
			refused := err != nil && strings.Contains(err.Error(), "failed to parse as URL")
			if got := pgURIRejected(dsn); got != refused {
				t.Fatalf("pgURIRejected(%q) = %v, the driver says %v (err = %v)", dsn, got, refused, err)
			}
		})
	}
}

// The URI that the driver corpus holds — every one a password is read from — is
// one the mirror must not call rejected: that would hide nothing and rewrite a
// message that holds no spelling of the secret.
func TestPgURIRejectedAcceptsWhatTheDriversParse(t *testing.T) {
	t.Parallel()

	for _, dsn := range driverCheckedDSNs {
		dsn := dsn
		if !isPostgresScheme(dsn) {
			continue
		}
		t.Run(dsn, func(t *testing.T) {
			t.Parallel()
			if pgURIRejected(dsn) {
				t.Fatalf("pgURIRejected(%q) = true, but the drivers read a password from it", dsn)
			}
		})
	}
}

func TestConfigRedactedHidesTheS3SecretKey(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Storage.S3.AccessKeyID = "AKIAEXAMPLE"
	cfg.Storage.S3.SecretAccessKey = "s3-super-secret" // #nosec G101 -- fake test secret.
	got := cfg.Redacted()
	if got.Storage.S3.SecretAccessKey != RedactedSecret {
		t.Fatalf("Redacted S3 secret = %q", got.Storage.S3.SecretAccessKey)
	}
	if got.Storage.S3.AccessKeyID != "AKIAEXAMPLE" {
		t.Fatalf("the access key id (not a secret) was changed: %q", got.Storage.S3.AccessKeyID)
	}
	if cfg.Storage.S3.SecretAccessKey != "s3-super-secret" {
		t.Fatal("Redacted modified the original config")
	}
}

func TestConfigRedactedHidesRedisPassword(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Database.DSN = "postgres://gombit:db-secret@localhost:5432/app" // #nosec G101 -- fake local test DSN.
	cfg.Cache.Redis.Password = "redis-secret"
	cfg.Auth.JWTSecret = "jwt-super-secret"

	got := cfg.Redacted()
	if strings.Contains(got.Database.DSN, "db-secret") {
		t.Fatalf("Redacted DSN = %q, still contains db password", got.Database.DSN)
	}
	if got.Cache.Redis.Password != RedactedSecret {
		t.Fatalf("Redacted Redis password = %q, want %s", got.Cache.Redis.Password, RedactedSecret)
	}
	if got.Auth.JWTSecret != RedactedSecret {
		t.Fatalf("Redacted JWT secret = %q, want %s", got.Auth.JWTSecret, RedactedSecret)
	}
	if cfg.Cache.Redis.Password != "redis-secret" {
		t.Fatal("Redacted() mutated the original Redis password")
	}
	if cfg.Auth.JWTSecret != "jwt-super-secret" {
		t.Fatal("Redacted() mutated the original JWT secret")
	}
}

func TestSanitizeErrorRemovesSecrets(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Database.DSN = "postgres://gombit:db-secret@localhost:5432/app" // #nosec G101 -- fake local test DSN.
	cfg.Cache.Redis.Password = "redis-secret"
	cfg.Auth.JWTSecret = "jwt-super-secret"

	err := errors.New("dial postgres://gombit:db-secret@localhost:5432/app with redis-secret jwt-super-secret")
	got := SanitizeError(err, cfg)
	if strings.Contains(got, "db-secret") || strings.Contains(got, "redis-secret") || strings.Contains(got, "jwt-super-secret") {
		t.Fatalf("SanitizeError() = %q, still contains secrets", got)
	}
}

// TestSanitizeErrorRemovesStorageSecrets: the storage secrets (the URL
// signing key and the S3 secret access key) are known secrets too.
func TestSanitizeErrorRemovesStorageSecrets(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Storage.URLSecret = "url-signing-secret-0123456789abcdef"
	cfg.Storage.S3.SecretAccessKey = "s3-secret-access-key"

	got := SanitizeError(errors.New("signing with url-signing-secret-0123456789abcdef as s3-secret-access-key"), cfg)
	if strings.Contains(got, "url-signing-secret") || strings.Contains(got, "s3-secret-access-key") {
		t.Fatalf("SanitizeError() = %q, still contains a storage secret", got)
	}
	if !strings.Contains(got, RedactedSecret) {
		t.Fatalf("SanitizeError() = %q, want the secrets redacted", got)
	}
}

func TestSanitizeErrorRemovesLibpqKeywordPassword(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Database.DSN = "host=localhost user=gombit password=s3cret dbname=app sslmode=disable" // #nosec G101 -- fake local test DSN.

	got := SanitizeError(errors.New("pq: password=s3cret authentication failed"), cfg)
	if strings.Contains(got, "s3cret") {
		t.Fatalf("SanitizeError() = %q, still contains libpq password", got)
	}
	if !strings.Contains(got, RedactedSecret) {
		t.Fatalf("SanitizeError() = %q, want redacted password", got)
	}
}

// minFuzzPasswordLen is how long a fuzzed password must be before its absence
// from the output is required: shorter ones are noise in any message.
const minFuzzPasswordLen = 4

// touchesTheFilesystem reports the settings whose parsers answer with a file:
// pgconn opens the file a passfile or ssl certificate/key names, and looks up
// a service file when `service` is set, so a fuzzer finds those spellings in
// seconds and the oracle stops being hermetic. driverCheckedDSNs keeps its own
// corpus offline for the same reason.
func touchesTheFilesystem(dsn string) bool {
	for _, key := range []string{"passfile", "sslrootcert", "sslcert", "sslkey", "service"} {
		if strings.Contains(dsn, key) {
			return true
		}
	}
	return false
}

// passwordFragments returns the texts whose absence from a printed string makes
// a password count as hidden: its whole, and — a password cut at the wrong
// separator leaves its end behind, and the shortest leak of that shape is the
// last three bytes — a tail of it.
func passwordFragments(password string) []string {
	fragments := []string{password}
	switch {
	case len(password) >= 8:
		fragments = append(fragments, password[len(password)/2:])
	case len(password) >= 4:
		fragments = append(fragments, password[len(password)-3:])
	}
	return fragments
}

// leakIsDistinguishable reports whether a leak of password would even show. A
// password whose bytes also spell part of what legitimately prints — the
// address outside the secret (a host that reads like the password, a user
// named after it, a twin parameter) or the fixed prose around it in a message
// template — cannot be told apart from that data in the output, so such an
// input says nothing about redaction and is skipped.
func leakIsDistinguishable(dsn, password, prose string) bool {
	var b strings.Builder
	previous := 0
	for _, r := range passwordRanges(dsn) {
		b.WriteString(dsn[previous:r[0]])
		previous = r[1]
	}
	b.WriteString(dsn[previous:])
	printed := b.String()
	for _, fragment := range passwordFragments(password) {
		if strings.Contains(printed, fragment) || strings.Contains(prose, fragment) {
			return false
		}
	}
	return true
}

// The hand-picked corpus passes through every leak a reviewer found, because it
// is only ever asked about inputs the author had thought of. The fuzzer
// generates the inputs instead: whatever password a driver takes from any
// string it accepts must be gone, tail and all (#486).
func FuzzRedactDSN(f *testing.F) {
	for _, dsn := range driverCheckedDSNs {
		f.Add(dsn)
	}
	f.Fuzz(func(t *testing.T, dsn string) {
		if touchesTheFilesystem(dsn) {
			return
		}
		printed := RedactDSN(dsn)
		for _, password := range driverPasswords(dsn) {
			if len(password) < minFuzzPasswordLen || !leakIsDistinguishable(dsn, password, "") {
				continue
			}
			assertPasswordHidden(t, dsn, "RedactDSN", printed, password)
		}
	})
}

// The same generated inputs against the message path: a message quoting the
// DSN, or the password the server rejected out of it, must lose both. The
// message is built from the DSN, the way a driver's error does, rather than
// fuzzed freely — a fragment of a password that turns up in free text on its
// own is not a spelling any grammar sends, and redaction has no marker to
// name it by.
func FuzzSanitizeError(f *testing.F) {
	for _, dsn := range driverCheckedDSNs {
		f.Add(dsn)
	}
	f.Fuzz(func(t *testing.T, dsn string) {
		if touchesTheFilesystem(dsn) {
			return
		}
		cfg := Default()
		cfg.Database.DSN = dsn // #nosec G101 -- fake fuzz DSN.
		printed := SanitizeError(errors.New("ping failed for "+dsn+" [rejected]"), cfg)
		for _, password := range driverPasswords(dsn) {
			if len(password) < minFuzzPasswordLen || !leakIsDistinguishable(dsn, password, "ping failed for  [rejected]") {
				continue
			}
			assertPasswordHidden(t, dsn, "SanitizeError", printed, password)
		}
	})
}

// The inverse property: redaction may only rewrite what it can name. A message
// holding neither the DSN, nor the bytes a grammar reads as a password, nor a
// password a driver reads from it, must come back unchanged — a spelling
// derived by a grammar that never applies it rewrote whole words of a message
// instead of the secret (#486). Three inputs are out of scope: a keyword/value
// string, whose quoting and backslash rules legitimately derive spellings that
// are neither the raw bytes nor the driver's value; a SQLite URI, whose
// parameter decoding has no parser in the oracle; and a non-postgres
// `scheme://` URL, whose userinfo decoding likewise has none.
func FuzzSanitizeErrorLeavesOtherTextAlone(f *testing.F) {
	for _, dsn := range driverCheckedDSNs {
		f.Add(dsn, "ping failed: the app is unreachable on the database host")
	}
	f.Fuzz(func(t *testing.T, dsn, text string) {
		if touchesTheFilesystem(dsn) || isFileURI(dsn) {
			return
		}
		if _, isPostgres, ok := cutScheme(dsn); ok && !isPostgres {
			return
		}
		// Only DSNs a driver actually reads a password out of are in scope: on
		// a look-alike key or a string every parser rejects, no password
		// reaches the wire, the cutting is fail-closed, and the spellings are
		// best-effort by design.
		if len(driverPasswords(dsn)) == 0 {
			return
		}
		// Only non-keyword DSNs are in scope: pgconn reads a password out of
		// everything else through its keyword grammar.
		if !isPostgresScheme(dsn) {
			if cfg, err := pgconn.ParseConfig(dsn); err == nil && cfg.Password != "" {
				return
			}
		}
		secrets := []string{dsn}
		for _, r := range secretRanges(dsn) {
			secrets = append(secrets, dsn[r.start:r.end])
		}
		secrets = append(secrets, driverPasswords(dsn)...)
		for _, secret := range secrets {
			if secret != "" && strings.Contains(text, secret) {
				return
			}
		}
		cfg := Default()
		cfg.Database.DSN = dsn // #nosec G101 -- fake fuzz DSN.
		if printed := SanitizeError(errors.New(text), cfg); printed != text {
			t.Fatalf("SanitizeError(%q) = %q, want it unchanged: the message holds no spelling of any secret of %q", text, printed, dsn)
		}
	})
}
