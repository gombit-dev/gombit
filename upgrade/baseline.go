// Package upgrade is Gombit's application upgrade tooling (UPGRADE-0): it
// works out where an application stands (its baseline), what moving it to
// another framework version requires, and proposes those changes for review.
// It never rewrites user-owned source without an explicit write action.
//
// The baseline is two facts. The framework version comes from the app's
// go.mod, read offline and never copied anywhere else; gombit contract app
// reads it the same way (internal/frameworkmod). When a go.work applies to
// the app, the workspace decides what it builds against, and no version is
// claimed. The scaffold version, the generation conventions the app was
// created with, is recorded by gombit new in a versioned block of
// gombit.yaml:
//
//	gombit:
//	  metadata: 1   # the format of this block
//	  scaffold: 1   # the scaffold conventions the app was generated with
//
// An app generated before that block existed has none: Detect reports it as
// scaffold 0, and RecordBaseline (gombit upgrade baseline --write) records
// that explicitly.
package upgrade

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/gombit-dev/gombit/internal/atomicfile"
	"github.com/gombit-dev/gombit/internal/frameworkmod"
)

// FrameworkModulePath is the module a Gombit application requires.
const FrameworkModulePath = frameworkmod.ModulePath

// MetadataVersion is the format of the gombit block in gombit.yaml that this
// framework reads and writes. A block with a newer format is refused
// (ErrMetadataTooNew): it was written by a newer framework, whose meaning this
// one cannot know.
const MetadataVersion = 1

// ScaffoldVersion is the version of the conventions gombit new generates
// with (the layout, the framework-owned files, their contents). It moves when
// those conventions change in a way an upgrade has to account for, not with
// every framework release. 0 is an app generated before upgrade metadata
// existed.
const ScaffoldVersion = 1

// ProjectFile is the file the scaffold version is recorded in, at the root of
// an application.
const ProjectFile = "gombit.yaml"

var (
	// ErrNotGombitApp: the directory has no go.mod requiring the framework.
	ErrNotGombitApp = errors.New("upgrade: not a Gombit application")
	// ErrMetadataTooNew: gombit.yaml's metadata format, or its scaffold
	// version, is newer than this framework understands.
	ErrMetadataTooNew = errors.New("upgrade: the upgrade metadata is newer than this gombit understands")
)

// Baseline is where an application stands: the framework it builds against
// and the conventions it was generated with.
type Baseline struct {
	// Framework is what go.mod says about the framework module.
	Framework Framework `json:"framework"`
	// Scaffold is the scaffold version the app was generated with: recorded
	// in gombit.yaml, or 0 when no metadata is recorded (Recorded false).
	Scaffold int `json:"scaffold"`
	// Metadata is the format of the recorded block (0 when there is none).
	Metadata int `json:"metadata"`
	// Recorded is whether gombit.yaml carries the metadata block. An app
	// without it was generated before upgrade metadata existed; its
	// baseline is detected (scaffold 0), and can be recorded with
	// RecordBaseline.
	Recorded bool `json:"recorded"`
}

// Replacement is a replace directive's target: a module path and version,
// or a local directory (no version).
type Replacement = frameworkmod.Replacement

// Framework is the framework dependency as the app's go.mod declares it.
type Framework struct {
	// Module is the framework's module path.
	Module string `json:"module"`
	// Version is the framework release the app builds against: the require
	// directive's version, or the version of a replace by the framework
	// module itself. It is empty when there is no framework release to
	// name: the framework is replaced by a local directory (Local) or by
	// another module (a fork: see Replace), or a go.work decides the build
	// (Workspace). Upgrade planning starts from Version, so it never names
	// what is not a release of the framework.
	Version string `json:"version,omitempty"`
	// Required is the require directive's version.
	Required string `json:"required"`
	// Replace is the replace directive in go.mod that applies to Required,
	// as the go command picks it, if any. Nil when a go.work applies
	// (Workspace): the workspace's replaces count then, and are not read.
	Replace *Replacement `json:"replace,omitempty"`
	// Local is true when go.mod replaces the framework by a local directory
	// (a framework checkout): there is no published version to upgrade
	// from. False when a go.work applies (Workspace), which decides instead.
	Local bool `json:"local,omitempty"`
	// Workspace is the go.work the go command uses for the app (its own
	// answer, `go env GOWORK`), if any. The workspace then decides the
	// framework the app builds against: its other modules' requirements and
	// replaces count too. This package does not resolve that, so only
	// Required (go.mod's) is set, and Version, Replace and Local are empty;
	// with GOWORK=off the app builds from its go.mod alone.
	Workspace string `json:"workspace,omitempty"`
}

// Detect reads the baseline of the application in workDir: the framework
// from go.mod, the scaffold version from gombit.yaml. It writes nothing.
// It asks the go command which go.work applies (`go env GOWORK`), so the go
// command must be on PATH.
func Detect(workDir string) (Baseline, error) {
	fw, err := detectFramework(workDir)
	if err != nil {
		return Baseline{}, err
	}
	meta, ok, err := readMetadata(workDir)
	if err != nil {
		return Baseline{}, err
	}
	b := Baseline{Framework: fw}
	if ok {
		b.Recorded = true
		b.Metadata = meta.Metadata
		b.Scaffold = meta.Scaffold
	}
	return b, nil
}

func detectFramework(workDir string) (Framework, error) {
	d, err := frameworkmod.Read(workDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Framework{}, fmt.Errorf("%w: no go.mod in %s", ErrNotGombitApp, workDir)
	case errors.Is(err, frameworkmod.ErrNotRequired):
		return Framework{}, fmt.Errorf("%w: %v", ErrNotGombitApp, err)
	case err != nil:
		return Framework{}, fmt.Errorf("upgrade: %w", err)
	}
	work, err := goWork(workDir)
	if err != nil {
		return Framework{}, err
	}
	fw := Framework{Module: FrameworkModulePath, Required: d.Required}
	if work != "" {
		// The workspace decides the build: go.mod's replace may not be the
		// one that applies, so it is not reported as such.
		fw.Workspace = work
		return fw, nil
	}
	fw.Version = d.Release()
	fw.Replace = d.Replace
	fw.Local = d.Replace != nil && d.Replace.Local()
	return fw, nil
}

// goWork is the go.work the go command uses for the app in workDir, or ""
// for none. It is the go command's own answer (`go env GOWORK`), so
// GOWORK=auto, a GOWORK set with `go env -w`, and a refused relative GOWORK
// behave here exactly as they do for go build.
func goWork(workDir string) (string, error) {
	cmd := exec.Command("go", "env", "GOWORK")
	cmd.Dir = workDir
	// The answer needs no other toolchain: never switch to (or download) the
	// one the app's go.mod asks for just to read it.
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return "", fmt.Errorf("upgrade: ask the go command which go.work applies (go env GOWORK): %w", err)
	}
	work := strings.TrimSpace(string(out))
	if work == "off" {
		return "", nil
	}
	return work, nil
}

// metadata is the gombit block of gombit.yaml.
type metadata struct {
	Metadata int
	Scaffold int
}

// readMetadata reads the gombit block of gombit.yaml; ok is false when the
// file or the block is absent.
func readMetadata(workDir string) (metadata, bool, error) {
	path := filepath.Join(workDir, ProjectFile)
	data, err := os.ReadFile(path) // #nosec G304 -- the app's project file
	if errors.Is(err, os.ErrNotExist) {
		return metadata{}, false, nil
	}
	if err != nil {
		return metadata{}, false, fmt.Errorf("upgrade: read %s: %w", path, err)
	}
	return parseMetadata(path, data)
}

// parseMetadata reads the gombit block from gombit.yaml content. The block's
// keys are exactly those of its format: metadata and scaffold, both integers
// (a float is not truncated into one, and an unknown key, such as a
// misspelling, is refused).
func parseMetadata(path string, data []byte) (metadata, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return metadata{}, false, fmt.Errorf("upgrade: %s: %w", path, err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return metadata{}, false, nil // an empty file
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return metadata{}, false, fmt.Errorf("upgrade: %s: want a mapping of keys at the top level", path)
	}
	var block *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "gombit" {
			continue
		}
		if block != nil {
			return metadata{}, false, fmt.Errorf("upgrade: %s: the gombit key appears twice", path)
		}
		block = root.Content[i+1]
	}
	switch {
	case block == nil:
		return metadata{}, false, nil
	case block.Kind == yaml.ScalarNode && block.ShortTag() == "!!null":
		// An empty gombit key is not "no metadata": recording a block
		// would then duplicate the key.
		return metadata{}, false, fmt.Errorf("upgrade: %s: the gombit key is empty (want the metadata block: gombit.metadata, gombit.scaffold)", path)
	case block.Kind != yaml.MappingNode:
		return metadata{}, false, fmt.Errorf("upgrade: %s: gombit must be the metadata block (gombit.metadata, gombit.scaffold)", path)
	}
	values := map[string]*yaml.Node{}
	var keys []string
	for i := 0; i+1 < len(block.Content); i += 2 {
		k := block.Content[i].Value
		if _, dup := values[k]; dup {
			return metadata{}, false, fmt.Errorf("upgrade: %s: gombit.%s appears twice", path, k)
		}
		values[k] = block.Content[i+1]
		keys = append(keys, k)
	}
	integer := func(key string) (int, bool, error) {
		n, ok := values[key]
		if !ok {
			return 0, false, nil
		}
		var v int
		if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!int" || n.Decode(&v) != nil {
			return 0, false, fmt.Errorf("upgrade: %s: gombit.%s is %q, want an integer", path, key, n.Value)
		}
		return v, true, nil
	}
	var m metadata
	format, ok, err := integer("metadata")
	switch {
	case err != nil:
		return metadata{}, false, err
	case !ok || format < 1:
		return metadata{}, false, fmt.Errorf("upgrade: %s: the gombit block has no metadata format (want gombit.metadata: %d)", path, MetadataVersion)
	case format > MetadataVersion:
		// Checked before the keys: a newer format may well have others.
		return metadata{}, false, fmt.Errorf("%w: %s has metadata format %d, this gombit reads up to %d; upgrade the gombit CLI", ErrMetadataTooNew, path, format, MetadataVersion)
	}
	m.Metadata = format
	for _, k := range keys {
		if k != "metadata" && k != "scaffold" {
			return metadata{}, false, fmt.Errorf("upgrade: %s: gombit.%s is not a key of metadata format %d (metadata, scaffold)", path, k, MetadataVersion)
		}
	}
	scaffold, ok, err := integer("scaffold")
	switch {
	case err != nil:
		return metadata{}, false, err
	case !ok:
		return metadata{}, false, fmt.Errorf("upgrade: %s: the gombit block has no scaffold version (want gombit.scaffold)", path)
	case scaffold < 0:
		return metadata{}, false, fmt.Errorf("upgrade: %s: gombit.scaffold is %d, want 0 or more", path, scaffold)
	case scaffold > ScaffoldVersion:
		return metadata{}, false, fmt.Errorf("%w: %s has scaffold version %d, this gombit knows up to %d; upgrade the gombit CLI", ErrMetadataTooNew, path, scaffold, ScaffoldVersion)
	}
	m.Scaffold = scaffold
	return m, true, nil
}

// MetadataBlock is the gombit.yaml block recording scaffold version scaffold,
// as gombit new writes it.
func MetadataBlock(scaffold int) string {
	return fmt.Sprintf(`# Upgrade baseline (gombit upgrade): the format of this block, and the
# scaffold conventions the app was generated with. The framework version is
# go.mod's, and is not repeated here.
gombit:
  metadata: %d
  scaffold: %d
`, MetadataVersion, scaffold)
}

// RecordBaseline records the detected baseline of an app that has none (one
// generated before upgrade metadata existed) by appending the metadata block,
// with scaffold 0, to its gombit.yaml (creating the file if there is none).
// Nothing else in the file changes: the result must read back as recorded,
// with every other key meaning what it did, or nothing is written. The file
// is replaced atomically. It reports whether it wrote; an app that already
// records its baseline is left as it is.
func RecordBaseline(workDir string) (Baseline, bool, error) {
	b, err := Detect(workDir)
	if err != nil || b.Recorded {
		return b, false, err
	}
	path := filepath.Join(workDir, ProjectFile)
	data, err := os.ReadFile(path) // #nosec G304 -- the app's project file
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return b, false, fmt.Errorf("upgrade: read %s: %w", path, err)
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	// A blank line before the block reads best, but it would join a
	// keep-chomped block scalar (|+, >+) ending the file; without it the
	// scalar may end cleanly. Use the first that leaves the rest unchanged.
	var lastErr error
	for _, sep := range []string{"\n", ""} {
		out := appended(data, sep)
		meta, ok, err := parseMetadata(path, out)
		switch {
		case err != nil:
			lastErr = err
			continue
		case !ok:
			lastErr = errors.New("the block would not be read back")
			continue
		case !sameExceptBlock(data, out):
			lastErr = errors.New("appending it would change the meaning of the file's last value")
			continue
		}
		if err := atomicfile.Write(path, out, mode); err != nil {
			return b, false, fmt.Errorf("upgrade: write %s: %w", path, err)
		}
		b.Recorded, b.Metadata, b.Scaffold = true, meta.Metadata, meta.Scaffold
		return b, true, nil
	}
	return b, false, fmt.Errorf("upgrade: %s cannot take the metadata block appended (%v); add it by hand:\n%s", path, lastErr, MetadataBlock(0))
}

// appended is data with the scaffold-0 metadata block appended after sep.
func appended(data []byte, sep string) []byte {
	var out bytes.Buffer
	out.Write(data)
	if len(data) > 0 {
		if data[len(data)-1] != '\n' {
			out.WriteByte('\n')
		}
		out.WriteString(sep)
	}
	out.WriteString(MetadataBlock(0))
	return out.Bytes()
}

// sameExceptBlock reports whether after decodes to before plus the gombit
// key, and nothing else changed.
func sameExceptBlock(before, after []byte) bool {
	var b, a any
	if yaml.Unmarshal(before, &b) != nil || yaml.Unmarshal(after, &a) != nil {
		return false
	}
	am, ok := a.(map[string]any)
	if !ok {
		return false
	}
	delete(am, "gombit")
	switch bm := b.(type) {
	case nil:
		return len(am) == 0
	case map[string]any:
		return reflect.DeepEqual(bm, am)
	default:
		return false
	}
}
