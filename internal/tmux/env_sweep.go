// Package tmux — environment-marker process sweep.
//
// Pane-tree enumeration (getPaneProcessTree, EnsurePIDsDead callers)
// only reaches processes still parented under the pane at kill time.
// Anything that daemonized earlier is invisible to it, but still
// carries the session's AGENTDECK_KILL_SCOPE marker in its inherited
// environment. This file finds those survivors by scanning same-user
// process environments and reaps them.
//
// Platform notes:
//   - Linux: /proc/<pid>/environ, NUL-separated, exact entry match.
//   - macOS: `ps -E` appends the environment to the command string with
//     no delimiter, and the tmux server's *argv* legitimately contains
//     the marker (`tmux … new-session … -e AGENTDECK_KILL_SCOPE=…`).
//     Matching therefore requires the token in the argv+env snapshot
//     AND its absence from the argv-only snapshot — otherwise the sweep
//     would kill the tmux server hosting every other session. macOS
//     additionally hides the environment of Apple platform binaries
//     (hardened runtime) from ps; the real sweep targets (node, bun,
//     go binaries) are readable.
//
// Never log raw scan output: `ps -E` lines contain complete process
// environments, including secrets. PIDs only.

package tmux

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SweepResult reports what an env-marker sweep saw and did.
type SweepResult struct {
	Matched   []int // every PID that carried the marker at any scan
	Killed    []int // matched PIDs verified gone by the end
	Survivors []int // matched PIDs still alive at deadline
}

// SweepProcessesWithEnvValue reaps same-user processes whose
// environment contains key=value (exact entry match). Repeated
// scan → signal rounds — SIGTERM first, SIGKILL after — until a scan
// comes back empty or the deadline expires. Rescanning globally
// between rounds catches children forked after the first scan; it
// cannot hit a restarted session because scope values are unique per
// launch. Excludes the calling process, its parent, and anything whose
// comm is tmux or agent-deck.
func SweepProcessesWithEnvValue(key, value string, timeout time.Duration) (SweepResult, error) {
	var res SweepResult
	if err := validateEnvToken(key, value); err != nil {
		return res, err
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	deadline := time.Now().Add(timeout)

	// Let `tmux kill-session`'s SIGHUP land before scanning: most of the
	// pane tree dies from that alone, and scanning too early would SIGTERM
	// processes that are already exiting.
	sleepUntilOrDuration(deadline, 500*time.Millisecond)

	matched := make(map[int]bool)
	sig := syscall.SIGTERM
	for {
		pids, err := findProcessesWithEnvValue(key, value)
		if err != nil {
			res.Survivors = setToSorted(matched)
			return res, err
		}
		if len(pids) == 0 {
			break
		}
		for _, pid := range pids {
			matched[pid] = true
		}
		respawnLog.Info("env_sweep_signal",
			slog.String("signal", sig.String()),
			slog.Int("count", len(pids)),
			slog.Any("pids", pids))
		for _, pid := range pids {
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Signal(sig)
			}
		}
		if time.Now().After(deadline) {
			res.Survivors = pids
			break
		}
		if sig == syscall.SIGTERM {
			sleepUntilOrDuration(deadline, 750*time.Millisecond)
			sig = syscall.SIGKILL
		} else {
			sleepUntilOrDuration(deadline, 250*time.Millisecond)
		}
	}

	res.Matched = setToSorted(matched)
	survivors := make(map[int]bool, len(res.Survivors))
	for _, pid := range res.Survivors {
		survivors[pid] = true
	}
	for _, pid := range res.Matched {
		if !survivors[pid] {
			res.Killed = append(res.Killed, pid)
		}
	}
	if len(res.Survivors) > 0 {
		respawnLog.Warn("env_sweep_survivors",
			slog.Int("count", len(res.Survivors)),
			slog.Any("pids", res.Survivors))
	}
	return res, nil
}

// validateEnvToken rejects keys/values that cannot form an exact,
// unambiguous environment entry. The value doubles as a ps-output
// token on macOS, so whitespace is forbidden there too.
func validateEnvToken(key, value string) error {
	if key == "" || value == "" {
		return fmt.Errorf("env sweep: empty key or value")
	}
	if strings.ContainsAny(key, "=\x00 \t\n") || strings.ContainsAny(value, "\x00 \t\n") {
		return fmt.Errorf("env sweep: key/value contains separator characters")
	}
	return nil
}

// findProcessesWithEnvValue returns same-user PIDs whose environment
// contains key=value, minus self/parent and excluded comms.
func findProcessesWithEnvValue(key, value string) ([]int, error) {
	var (
		pids []int
		err  error
	)
	switch runtime.GOOS {
	case "linux":
		pids, err = findEnvMatchesProcFS(key, value)
	case "darwin":
		pids, err = findEnvMatchesPS(key, value)
	default:
		return nil, fmt.Errorf("env sweep unsupported on %s", runtime.GOOS)
	}
	if err != nil {
		return nil, err
	}
	self, parent := os.Getpid(), os.Getppid()
	out := pids[:0]
	for _, pid := range pids {
		if pid == self || pid == parent || pid <= 1 {
			continue
		}
		if isExcludedComm(pid) {
			continue
		}
		out = append(out, pid)
	}
	return out, nil
}

// findEnvMatchesProcFS scans /proc on Linux. Per-pid failures are
// process-exit races and are skipped; only a failure to read /proc
// itself is an error.
func findEnvMatchesProcFS(key, value string) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("env sweep: reading /proc: %w", err)
	}
	token := []byte(key + "=" + value)
	uid := uint32(os.Geteuid())
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		// Same-user filter by /proc/<pid> ownership — do not treat
		// "unreadable" as "other user" (root/capabilities break that).
		info, err := os.Stat("/proc/" + e.Name())
		if err != nil {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uid {
			continue
		}
		environ, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil {
			continue
		}
		for _, entry := range splitNUL(environ) {
			if string(entry) == string(token) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, nil
}

func splitNUL(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == 0 {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// findEnvMatchesPS scans on macOS via two ps snapshots keyed by PID.
// A PID qualifies only if the token appears in its argv+env line and
// NOT in its argv-only line; a PID missing from either snapshot
// (appeared/exited between them) is skipped — fail safe.
func findEnvMatchesPS(key, value string) ([]int, error) {
	withEnv, err := exec.Command("ps", "-xww", "-E", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("env sweep: ps -E: %w", err)
	}
	argvOnly, err := exec.Command("ps", "-xww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("env sweep: ps: %w", err)
	}
	return diffEnvMatches(string(withEnv), string(argvOnly), key+"="+value), nil
}

// diffEnvMatches is the pure core of the macOS scanner, split out for
// fixture tests.
func diffEnvMatches(withEnv, argvOnly, token string) []int {
	envLines := parsePSPidCommand(withEnv)
	argvLines := parsePSPidCommand(argvOnly)
	var pids []int
	for pid, line := range envLines {
		if !containsToken(line, token) {
			continue
		}
		argvLine, ok := argvLines[pid]
		if !ok || containsToken(argvLine, token) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

func parsePSPidCommand(out string) map[int]string {
	m := make(map[int]string)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimLeft(line, " \t")
		if line == "" {
			continue
		}
		sp := strings.IndexAny(line, " \t")
		if sp < 0 {
			continue
		}
		pid, err := strconv.Atoi(line[:sp])
		if err != nil {
			continue
		}
		m[pid] = line[sp+1:]
	}
	return m
}

// containsToken reports whether token appears in s bounded by
// whitespace or string edges — `ps -E` separates entries with spaces
// and the token itself contains none (validateEnvToken).
func containsToken(s, token string) bool {
	idx := 0
	for {
		i := strings.Index(s[idx:], token)
		if i < 0 {
			return false
		}
		i += idx
		leftOK := i == 0 || s[i-1] == ' ' || s[i-1] == '\t'
		right := i + len(token)
		rightOK := right == len(s) || s[right] == ' ' || s[right] == '\t'
		if leftOK && rightOK {
			return true
		}
		idx = i + 1
	}
}

// isExcludedComm protects infrastructure processes that legitimately
// carry the marker: the tmux server (hosts every session; its env can
// contain the marker when the server was first started from inside a
// managed session) and agent-deck itself.
func isExcludedComm(pid int) bool {
	var comm string
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err != nil {
			return true // vanished or unreadable — don't signal it
		}
		comm = strings.TrimSpace(string(b))
	case "darwin":
		out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "ucomm=").Output()
		if err != nil {
			return true
		}
		comm = strings.TrimSpace(string(out))
	default:
		return true
	}
	base := filepath.Base(comm)
	return base == "tmux" || strings.HasPrefix(base, "tmux:") || base == "agent-deck"
}

func setToSorted(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for pid := range set {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}
