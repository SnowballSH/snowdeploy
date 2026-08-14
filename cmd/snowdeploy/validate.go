package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/SnowballSH/snowdeploy/internal/manifest"
)

// scannerType names the SSE reader so the follow loop's signature stays honest
// about what it consumes.
type scannerType = bufio.Scanner

// cmdValidate parses and checks every manifest in a directory. It needs no
// daemon and no credential, which is what makes it usable from CI.
func cmdValidate(args []string, stdout io.Writer) error {
	fs := newFlagSet("validate", stdout)
	volumePrefixes := fs.String("volume-prefixes", "",
		"comma-separated host path prefixes volumes may use (empty skips the check)")
	envFilePrefixes := fs.String("env-file-prefixes", "",
		"comma-separated host path prefixes env files may use (empty skips the check)")
	dir, err := parseWithOperand(fs, args)
	if err != nil {
		return err
	}

	lim := manifest.Limits{
		VolumePrefixes:  splitList(*volumePrefixes),
		EnvFilePrefixes: splitList(*envFilePrefixes),
	}

	files, err := manifestFiles(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no *.yaml manifests found in %s", dir)
	}

	var failures int
	for _, path := range files {
		if err := checkOne(path, lim); err != nil {
			failures++
			fmt.Fprintf(stdout, "FAIL %s\n", path)
			for _, line := range strings.Split(err.Error(), "\n") {
				fmt.Fprintf(stdout, "       %s\n", line)
			}
			continue
		}
		fmt.Fprintf(stdout, "ok   %s\n", path)
	}

	if failures > 0 {
		return fmt.Errorf("%d of %d manifests failed validation", failures, len(files))
	}
	fmt.Fprintf(stdout, "\n%d manifests validated.\n", len(files))
	return nil
}

func manifestFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read manifest directory: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	return files, nil
}

func checkOne(path string, lim manifest.Limits) error {
	raw, err := os.ReadFile(path) // #nosec G304 -- path came from the named directory
	if err != nil {
		return err
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return err
	}
	if err := m.Validate(lim); err != nil {
		return err
	}
	if name := strings.TrimSuffix(filepath.Base(path), ".yaml"); m.Name != name {
		return fmt.Errorf(
			"name %q does not match the file name %q; the daemon addresses services by file name",
			m.Name, name)
	}
	return nil
}

func splitList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
