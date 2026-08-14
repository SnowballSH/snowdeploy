package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/SnowballSH/snowdeploy/internal/manifest"
)

// Phases an Applier reports as it works, so a caller can stream honest
// progress without reimplementing the sequence.
const (
	PhaseRestarting = "restarting"
	PhaseProbing    = "probing"
)

// PhaseFunc receives each phase as it begins. It may be nil.
type PhaseFunc func(phase string)

// Applier walks one manifest onto the host. The order is load-bearing: the
// rendered unit is validated before it is written, so a template that escapes
// the manifest rules never reaches the unit directory at all.
type Applier struct {
	S             Systemd
	Lim           manifest.Limits
	ProbeInterval time.Duration
}

// Apply renders, validates, writes, restarts, and probes.
func (a *Applier) Apply(
	ctx context.Context, m *manifest.Manifest, tmpl string, onPhase PhaseFunc,
) error {
	report := func(phase string) {
		if onPhase != nil {
			onPhase(phase)
		}
	}
	if err := m.Validate(a.Lim); err != nil {
		return fmt.Errorf("manifest %s is invalid: %w", m.Name, err)
	}

	unit, err := Render(tmpl, m)
	if err != nil {
		return fmt.Errorf("render %s: %w", m.Name, err)
	}
	if err := manifest.ValidateRendered(unit); err != nil {
		return fmt.Errorf("rendered unit for %s is unsafe: %w", m.Name, err)
	}

	if err := a.S.WriteUnit(UnitName(m.Name), unit); err != nil {
		return fmt.Errorf("write unit for %s: %w", m.Name, err)
	}
	report(PhaseRestarting)
	if err := a.S.ReloadAndRestart(ctx, m.Name); err != nil {
		return fmt.Errorf("restart %s: %w", m.Name, err)
	}

	report(PhaseProbing)
	if err := Probe(ctx, m.Health.URL, m.Health.Timeout, a.ProbeInterval); err != nil {
		return fmt.Errorf("%s did not become healthy: %w", m.Name, err)
	}
	return nil
}
