package scaffold

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

func (opts *Options) resolveInteractive() error {
	needName := opts.Name == ""
	if !needName {
		return nil
	}
	tty := false
	if opts.IsTTY != nil {
		tty = opts.IsTTY()
	} else {
		tty = stdinIsTTY(opts.Stdin)
	}
	if !tty {
		return fmt.Errorf("scaffold: project name is required (pass a name or run interactively on a TTY)")
	}

	in := bufio.NewReader(opts.Stdin)

	name, err := promptLine(opts.Stdout, in, "Project name", "")
	if err != nil {
		return err
	}
	opts.Name = name

	// Prompt answers are normalized with the same rules as flag values, so a
	// capitalized answer such as "Postgres" is accepted the same way
	// `--database Postgres` is.
	database, err := promptLine(opts.Stdout, in, "Database (sqlite, postgres, mysql)", opts.Database)
	if err != nil {
		return err
	}
	opts.Database = strings.ToLower(strings.TrimSpace(database))

	cache, err := promptLine(opts.Stdout, in, "Cache (memory, redis, noop)", opts.Cache)
	if err != nil {
		return err
	}
	opts.Cache = strings.ToLower(strings.TrimSpace(cache))

	auth, err := promptLine(opts.Stdout, in, "Auth (jwt, cookie)", opts.Auth)
	if err != nil {
		return err
	}
	opts.Auth = strings.ToLower(strings.TrimSpace(auth))

	ui, err := promptLine(opts.Stdout, in, "UI (minimal, mui)", opts.UI)
	if err != nil {
		return err
	}
	opts.UI = strings.ToLower(strings.TrimSpace(ui))

	if opts.Module == "" {
		module, err := promptLine(opts.Stdout, in, "Go module path", DefaultModulePrefix+"/"+opts.Name)
		if err != nil {
			return err
		}
		opts.Module = module
	}
	return nil
}

func promptLine(out io.Writer, in *bufio.Reader, question, def string) (string, error) {
	if def != "" {
		if _, err := fmt.Fprintf(out, "%s [%s]: ", question, def); err != nil {
			return "", err
		}
	} else {
		if _, err := fmt.Fprintf(out, "%s: ", question); err != nil {
			return "", err
		}
	}
	line, err := in.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("scaffold: read prompt: %w", err)
	}
	if err == io.EOF && strings.TrimSpace(line) == "" && def == "" {
		return "", fmt.Errorf("scaffold: read prompt: EOF")
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}
