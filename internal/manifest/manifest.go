// Package manifest defines the declarative service description snowdeploy
// reconciles, together with the safety rules a manifest and the Quadlet unit
// rendered from it must satisfy before anything reaches the host.
package manifest

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultHealthTimeout bounds a health probe when a manifest names none.
const DefaultHealthTimeout = 30 * time.Second

// Manifest is one managed service.
type Manifest struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	Image    Image             `yaml:"image"`
	Template string            `yaml:"template"`
	Port     int               `yaml:"port"`
	Network  string            `yaml:"network"`
	Env      map[string]string `yaml:"env"`
	EnvFiles []string          `yaml:"env_files"`
	Volumes  []string          `yaml:"volumes"`
	Health   Health            `yaml:"health"`
}

// UnitDescription is what the rendered unit's Description= should say. It
// falls back to the service name so the field stays optional.
func (m *Manifest) UnitDescription() string {
	if strings.TrimSpace(m.Description) != "" {
		return m.Description
	}
	return m.Name
}

// Image pins the container image. Digest pins only; tags are rejected.
type Image struct {
	Repository string `yaml:"repository"`
	Digest     string `yaml:"digest"`
}

// Ref is the fully qualified digest reference a rendered unit must carry.
func (i Image) Ref() string {
	return i.Repository + "@" + i.Digest
}

// Health describes the post-restart readiness probe.
type Health struct {
	URL     string        `yaml:"url"`
	Timeout time.Duration `yaml:"timeout"`
}

// Limits are the host-side allowlists a daemon supplies from its configuration.
// An empty slice disables that check, which is what the standalone validator
// uses when it has no host context.
type Limits struct {
	VolumePrefixes  []string
	EnvFilePrefixes []string
}

// Parse decodes one manifest document, rejecting unknown fields so a typo can
// never silently disable a setting, and applies defaults.
func Parse(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	m.applyDefaults()
	return &m, nil
}

// Marshal re-encodes a manifest in the on-disk form.
func Marshal(m *Manifest) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return buf.Bytes(), nil
}

func (m *Manifest) applyDefaults() {
	if m.Health.Timeout == 0 {
		m.Health.Timeout = DefaultHealthTimeout
	}
}
