// Package tmux — per-launch kill-scope marker.
//
// Children spawned inside a session inherit its environment. A process
// that setsid/daemonizes (nx daemon, dev servers) leaves the pane
// process tree before kill time, so pane-tree enumeration can never
// find it — but the inherited environment survives reparenting to
// PID 1. Each `tmux new-session` gets a fresh AGENTDECK_KILL_SCOPE
// value via `-e`, which (a) lands in every pane descendant's
// environment, (b) is readable back through `tmux show-environment`
// at kill time, and (c) overrides any marker inherited from an outer
// agent-deck session, so deleting a parent deck session can never
// sweep a nested one. A fresh value per launch also makes the sweep
// generation-safe: a restart mints a new scope, so a late async sweep
// of the old scope cannot touch the new process tree.

package tmux

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// KillScopeEnvKey is the environment variable carrying the per-launch
// kill-scope marker. Exported so the session layer can read it back
// via Session.GetEnvironment before teardown.
const KillScopeEnvKey = "AGENTDECK_KILL_SCOPE"

var (
	newSessionEnvFlagOnce sync.Once
	newSessionEnvFlagOK   bool
)

// supportsNewSessionEnvFlag reports whether the local tmux accepts
// `new-session -e KEY=VAL` (added in tmux 3.2). Fails closed on probe
// or parse failure: no injection, no sweep — pre-existing behavior.
func supportsNewSessionEnvFlag() bool {
	newSessionEnvFlagOnce.Do(func() {
		raw, err := defaultTmuxVersionProbe()
		if err != nil {
			return
		}
		newSessionEnvFlagOK = versionSupportsNewSessionEnvFlag(parseTmuxVersion(raw))
	})
	return newSessionEnvFlagOK
}

func versionSupportsNewSessionEnvFlag(ver string) bool {
	if ver == "master" || ver == "next" {
		return true
	}
	major, minor, _, ok := splitTmuxVersion(ver)
	if !ok {
		return false
	}
	return major > 3 || (major == 3 && minor >= 2)
}

// newKillScopeValue mints a unique per-launch scope value. The instance
// ID prefix is for debuggability (ps/env output points back to the
// session); the nonce is what makes it generation-unique. Returns ""
// (no injection) if the system RNG fails.
func newKillScopeValue(instanceID string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	nonce := hex.EncodeToString(b[:])
	if instanceID == "" {
		return "scope-" + nonce
	}
	return instanceID + "." + nonce
}
