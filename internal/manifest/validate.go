package manifest

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// digestPattern is the only image pin a manifest may carry.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Validate enforces the manifest half of the safety model: digest pins only,
// a sane loopback port, and host paths confined to the configured prefixes.
func (m *Manifest) Validate(lim Limits) error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if strings.TrimSpace(m.Name) == "" {
		fail("name is required")
	}
	if strings.TrimSpace(m.Template) == "" {
		fail("template is required")
	}
	if err := validateRepository(m.Image.Repository); err != nil {
		fail("image.repository: %w", err)
	}
	if !digestPattern.MatchString(m.Image.Digest) {
		fail("image.digest %q is not a sha256 digest pin (tags are rejected)", m.Image.Digest)
	}
	if m.Port < 1 || m.Port > 65535 {
		fail("port %d is outside 1-65535", m.Port)
	}
	if err := validateHealthURL(m.Health.URL); err != nil {
		fail("health.url: %w", err)
	}
	if m.Health.Timeout < 0 {
		fail("health.timeout %v is negative", m.Health.Timeout)
	}

	for _, v := range m.Volumes {
		host, _, _ := strings.Cut(v, ":")
		if err := validateHostPath(host, lim.VolumePrefixes); err != nil {
			fail("volume %q: %w", v, err)
		}
	}
	for _, f := range m.EnvFiles {
		if err := validateHostPath(f, lim.EnvFilePrefixes); err != nil {
			fail("env_file %q: %w", f, err)
		}
	}

	return errors.Join(errs...)
}

func validateRepository(repo string) error {
	if strings.TrimSpace(repo) == "" {
		return errors.New("is required")
	}
	if strings.Contains(repo, "@") {
		return errors.New("must not embed a digest; use image.digest")
	}
	// A colon is legitimate in a registry host:port, which is never the last
	// path element. A colon in the last element is a tag.
	last := repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		last = repo[i+1:]
	}
	if strings.Contains(last, ":") {
		return errors.New("must not carry a tag; digest pins only")
	}
	return nil
}

func validateHealthURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("is not a URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("has no host")
	}
	return nil
}

// validateHostPath confines a host path to one of the allowed prefixes. No
// prefixes configured means the caller has no host context and the check is
// deliberately skipped.
func validateHostPath(p string, prefixes []string) error {
	if len(prefixes) == 0 {
		return nil
	}
	if !strings.HasPrefix(p, "/") {
		return errors.New("is not an absolute host path")
	}
	clean := path.Clean(p)
	for _, prefix := range prefixes {
		if strings.HasPrefix(clean, path.Clean(prefix)+"/") {
			return nil
		}
	}
	return fmt.Errorf("resolves to %s, outside the allowed prefixes %v", clean, prefixes)
}

// forbiddenKeys are Quadlet keys no rendered unit may ever carry.
var forbiddenKeys = map[string]string{
	"AddCapability":        "adds a Linux capability",
	"Privileged":           "requests a privileged container",
	"AddDevice":            "passes a host device through",
	"SecurityLabelDisable": "disables SELinux confinement",
}

// forbiddenPodmanArgs are privilege escapes that would otherwise ride in on the
// Quadlet escape hatch.
var forbiddenPodmanArgs = []string{
	"--privileged",
	"--cap-add",
	"--userns",
	"--pid=host",
	"--ipc=host",
	"--network=host",
	"--net=host",
	"--security-opt",
	"--device",
	"--publish",
	"-p ",
}

var loopbackPublishPrefixes = []string{"127.0.0.1:", "[::1]:"}

// ValidateRendered is the gate between rendering a unit and writing it. It
// reads the unit exactly as systemd would, so a template cannot smuggle a
// privilege escalation or a public listener past the manifest checks.
func ValidateRendered(unit string) error {
	var errs []error

	for i, line := range strings.Split(unit, "\n") {
		lineNo := i + 1
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if why, bad := forbiddenKeys[key]; bad {
			errs = append(errs, fmt.Errorf("line %d: %s= %s", lineNo, key, why))
			continue
		}

		switch key {
		case "PublishPort":
			if !hasAnyPrefix(value, loopbackPublishPrefixes) {
				errs = append(errs, fmt.Errorf(
					"line %d: PublishPort=%s is not loopback-only", lineNo, value))
			}
		case "Network":
			if value == "host" {
				errs = append(errs, fmt.Errorf(
					"line %d: Network=host defeats the loopback-only publish rule", lineNo))
			}
		case "Image":
			if !strings.Contains(value, "@sha256:") {
				errs = append(errs, fmt.Errorf(
					"line %d: Image=%s is not pinned by digest", lineNo, value))
			}
		case "PodmanArgs":
			for _, arg := range forbiddenPodmanArgs {
				if strings.Contains(value+" ", arg) {
					errs = append(errs, fmt.Errorf(
						"line %d: PodmanArgs carries %s", lineNo, strings.TrimSpace(arg)))
				}
			}
		}
	}

	return errors.Join(errs...)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
