// Package deploy is the merge-first state machine: it proposes a manifest
// change, waits for the configuration repository's own checks, merges, applies
// the merged state to the host, probes, and journals — rolling back and
// reverting main when the probe fails. Converge is the one action that skips
// the proposal: it re-applies what main already holds.
package deploy

import "github.com/SnowballSH/snowdeploy/internal/journal"

// The nine states a deploy moves through. The terminal three are the journal's,
// so a receipt and a live event can never disagree about what "healthy" means.
const (
	StateDetected    = "detected"
	StatePROpen      = "pr-open"
	StateChecks      = "checks"
	StateMerged      = "merged"
	StateReconciling = "reconciling"
	StateProbing     = "probing"
	StateHealthy     = journal.StateHealthy
	StateRolledBack  = journal.StateRolledBack
	StateFailed      = journal.StateFailed
)

// States lists every state in lifecycle order, for the UI progress rail.
var States = []string{
	StateDetected, StatePROpen, StateChecks, StateMerged,
	StateReconciling, StateProbing, StateHealthy,
}

// isTerminal reports whether a state ends a deploy.
func isTerminal(state string) bool {
	switch state {
	case StateHealthy, StateRolledBack, StateFailed:
		return true
	default:
		return false
	}
}
