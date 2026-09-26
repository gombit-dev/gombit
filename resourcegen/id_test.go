package resourcegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestUUIDPrimaryKeyModel(t *testing.T) {
	name, err := parseResourceName("Session")
	if err != nil {
		t.Fatal(err)
	}
	ctx := newRenderContext("example.com/demo", name, nil, "/api/v1", "minimal", false, false)
	ctx.IDStrategy = idUUID
	src := renderModel(ctx)
	for _, want := range []string{
		`ID        uuid.UUID      ` + "`" + `gorm:"type:char(36);primaryKey" json:"id" gombit:"read,server"` + "`",
		"func (m *Session) BeforeCreate(*gorm.DB) error",
		"uuid.New()",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("model missing %q:\n%s", want, src)
		}
	}
	if strings.Contains(src, "gorm.Model") {
		t.Fatalf("uuid model still embeds gorm.Model:\n%s", src)
	}

	root := resourcegenModuleRoot(t)
	dir := filepath.Join(root, "internal", "_uuidsession")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-mod=readonly", "./internal/_uuidsession")
	build.Dir = root
	build.Env = append(os.Environ(), "GOPROXY=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s\n%s", err, out, src)
	}
}

func TestDefaultPrimaryKeyStaysGormModel(t *testing.T) {
	name, err := parseResourceName("Widget")
	if err != nil {
		t.Fatal(err)
	}
	ctx := newRenderContext("example.com/demo", name, nil, "/api/v1", "minimal", false, false)
	src := renderModel(ctx)
	if !strings.Contains(src, "gorm.Model") || strings.Contains(src, "uuid.UUID") {
		t.Fatalf("default model:\n%s", src)
	}
}

func TestLookupTargetPrimaryKey(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "internal", "user")
	if err := os.MkdirAll(user, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "package user\n\nimport \"github.com/google/uuid\"\n\ntype User struct {\n\tID uuid.UUID `gorm:\"type:char(36);primaryKey\"`\n}\n"
	if err := os.WriteFile(filepath.Join(user, "user.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := lookupTargetPK(dir, "user", "User")
	if err != nil || got != "uuid.UUID" {
		t.Fatalf("lookup = %q %v", got, err)
	}
	missing, err := lookupTargetPK(dir, "absent", "Absent")
	if err != nil || missing != "uint" {
		t.Fatalf("missing target = %q %v", missing, err)
	}

	fields, err := parseFieldsWithID([]string{"author:belongs_to:User"}, "post", func(pkg, typeName string) (string, error) {
		return lookupTargetPK(dir, pkg, typeName)
	})
	if err != nil {
		t.Fatal(err)
	}
	if fields[0].fkColumnGoType() != "uuid.UUID" {
		t.Fatalf("fk = %q", fields[0].fkColumnGoType())
	}
	lines := modelFieldLines(fields[0], "post")
	if !strings.Contains(lines, "AuthorID uuid.UUID") || !strings.Contains(lines, "type:char(36);index") || strings.Contains(lines, "filterable") {
		t.Fatalf("uuid fk lines:\n%s", lines)
	}

	bad := filepath.Join(dir, "internal", "note")
	if err := os.MkdirAll(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "note.go"), []byte("package note\n\ntype Note struct {\n\tID string `gorm:\"primaryKey\"`\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lookupTargetPK(dir, "note", "Note"); err == nil {
		t.Fatal("string primary key was accepted")
	}
}

type uuidResource struct {
	ID        uuid.UUID `gorm:"type:char(36);primaryKey" json:"id" gombit:"read,server"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt gorm.DeletedAt `gorm:"index"`
	Name      string         `gorm:"not null"`
}

func TestUUIDHandlerParsesUUID(t *testing.T) {
	res, err := buildModelResource(&uuidResource{}, "uuidresource")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	src, err := renderModelHandler(res)
	if err != nil {
		t.Fatalf("renderModelHandler: %v", err)
	}
	for _, want := range []string{`format:"uuid"`, "uuid.Parse(input.ID)", `First(&row, "id = ?", id)`} {
		if !strings.Contains(src, want) {
			t.Fatalf("handler missing %q:\n%s", want, src)
		}
	}
	if strings.Contains(src, "ParseUint") {
		t.Fatal("uuid handler still parses a uint")
	}
}

type compositeKey struct {
	A    uint `gorm:"primaryKey"`
	B    uint `gorm:"primaryKey"`
	Name string
}

func TestCompositePrimaryKeyRejected(t *testing.T) {
	if _, err := buildModelResource(&compositeKey{}, "compositekey"); err == nil || !strings.Contains(err.Error(), "composite primary key") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseIDStrategy(t *testing.T) {
	id, err := parseIDStrategy("")
	if err != nil || id != idUint {
		t.Fatalf("empty = %q %v", id, err)
	}
	id, err = parseIDStrategy("uuid")
	if err != nil || id != idUUID {
		t.Fatalf("uuid = %q %v", id, err)
	}
	if _, err := parseIDStrategy("ulid"); err == nil {
		t.Fatal("ulid was accepted")
	}
}
