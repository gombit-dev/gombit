package resourcegen

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/types"
)

// A file field the request omits (gombit:"read" or "read,server") is described in
// the response, so the generated DTO must not import types for it: nothing uses
// the import. Likewise a write-only file field (gombit:"write") has no response
// description, so the DTO must not import filefield.
type readServerFileModel struct {
	ID    uint       `gorm:"primaryKey"`
	Title string     `gombit:"read,write"`
	Thumb types.File `gorm:"size:512;uniqueIndex" gombit:"read,server"`
}

type writeOnlyFileModel struct {
	ID         uint       `gorm:"primaryKey"`
	Title      string     `gombit:"read,write"`
	Attachment types.File `gorm:"size:512;uniqueIndex" gombit:"write"`
}

func TestFileFieldOutsideRequestImportsOnlyWhatIsUsed(t *testing.T) {
	const typesImport = `"github.com/gombit-dev/gombit/types"`
	const filefieldImport = `"github.com/gombit-dev/gombit/storage/filefield"`

	res, err := buildModelResource(&readServerFileModel{}, "readserverpkg")
	if err != nil {
		t.Fatalf("buildModelResource(read,server): %v", err)
	}
	readServerDTO := string(mustFormatGo(renderModelDTOs(res)))
	if strings.Contains(readServerDTO, typesImport) {
		t.Fatalf("read,server DTO imports types, but no DTO line uses it:\n%s", readServerDTO)
	}
	if !strings.Contains(readServerDTO, filefieldImport) {
		t.Fatalf("read,server DTO must import filefield for its FileInfo response:\n%s", readServerDTO)
	}
	readServerHandler, err := renderModelHandler(res)
	if err != nil {
		t.Fatalf("renderModelHandler(read,server): %v", err)
	}
	assertFileModelCompiles(t, res, "readserverpkg",
		fmt.Sprintf("package readserverpkg\n\nimport \"github.com/gombit-dev/gombit/types\"\n\ntype %s struct {\n\tID    uint `gorm:\"primaryKey\"`\n\tTitle string\n\tThumb types.File `gorm:\"size:512;uniqueIndex\"`\n}\n", res.TypeName),
		readServerDTO, readServerHandler)

	res, err = buildModelResource(&writeOnlyFileModel{}, "writeonlypkg")
	if err != nil {
		t.Fatalf("buildModelResource(write): %v", err)
	}
	writeDTO := string(mustFormatGo(renderModelDTOs(res)))
	if strings.Contains(writeDTO, filefieldImport) {
		t.Fatalf("write-only DTO imports filefield, but no DTO line uses it:\n%s", writeDTO)
	}
	if !strings.Contains(writeDTO, typesImport) {
		t.Fatalf("write-only DTO must import types for its request field:\n%s", writeDTO)
	}
	writeHandler, err := renderModelHandler(res)
	if err != nil {
		t.Fatalf("renderModelHandler(write): %v", err)
	}
	if strings.Contains(writeHandler, "var err error") {
		t.Fatalf("write-only handler declares err with no response file to resolve:\n%s", writeHandler)
	}
	assertFileModelCompiles(t, res, "writeonlypkg",
		fmt.Sprintf("package writeonlypkg\n\nimport \"github.com/gombit-dev/gombit/types\"\n\ntype %s struct {\n\tID         uint `gorm:\"primaryKey\"`\n\tTitle      string\n\tAttachment types.File `gorm:\"size:512;uniqueIndex\"`\n}\n", res.TypeName),
		writeDTO, writeHandler)
}

// assertFileModelCompiles builds the rendered DTO, handler and hooks, with the
// model they were generated for, in a throwaway module against this checkout.
// Substring checks cannot show that the output compiles; this does.
func assertFileModelCompiles(t *testing.T, res modelResource, pkg, model, dto, handler string) {
	t.Helper()
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, pkg)
	if err := os.MkdirAll(pkgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"),
		"module fileoutsideapp\n\ngo 1.23\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => "+resourcegenModuleRoot(t)+"\n")
	writeFile(t, filepath.Join(pkgDir, "model.go"), model)
	writeFile(t, filepath.Join(pkgDir, "dto.gen.go"), dto)
	writeFile(t, filepath.Join(pkgDir, "handler.gen.go"), handler)
	writeFile(t, filepath.Join(pkgDir, "hooks.go"), string(mustFormatGo(renderModelHooks(res))))
	if out, err := tidyThrowawayModule(dir); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated %s package does not compile: %v\n%s\n--- dto ---\n%s\n--- handler ---\n%s", pkg, err, out, dto, handler)
	}
}
