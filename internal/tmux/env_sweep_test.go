// Tests for the env-marker orphan sweep (env_sweep.go, kill_scope.go).
//
// The detached-orphan tests re-exec this test binary as the marked
// process instead of using /bin/sleep: on macOS, ps -E cannot read the
// environment of Apple platform binaries (hardened runtime), so a
// sleep-based fixture would pass vacuously on Linux and fail on
// Darwin. The test binary itself is ad-hoc signed and readable.
package tmux

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	envSweepHelperMode = "ENV_SWEEP_HELPER_MODE"
	envSweepHelperEnv  = "ENV_SWEEP_HELPER_MARKER" // KEY=VALUE to give the grandchild
)

// TestHelperEnvSweep is not a real test: it is the body of the helper
// processes spawned by the tests below. Skipped in normal runs.
func TestHelperEnvSweep(t *testing.T) {
	switch os.Getenv(envSweepHelperMode) {
	case "":
		t.Skip("helper entrypoint, not a test")
	case "sleep":
		// Lifetime cap so a failed assertion can't leak this process
		// past the test run. Dies to SIGTERM via the default handler.
		time.Sleep(120 * time.Second)
	case "orphan":
		// Spawn a marked grandchild in its own session, report its PID,
		// and exit — the grandchild reparents to PID 1, reproducing a
		// daemonized dev server that left the pane tree. The marker
		// either arrives explicitly (unit tests, via envSweepHelperEnv)
		// or is already inherited (e2e test, injected into the pane by
		// tmux new-session -e) — inherited env is passed through.
		exe, err := os.Executable()
		if err != nil {
			fmt.Println("ERR:", err)
			return
		}
		marker := os.Getenv(envSweepHelperEnv)
		var env []string
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, envSweepHelperMode+"=") ||
				strings.HasPrefix(kv, envSweepHelperEnv+"=") {
				continue
			}
			if marker != "" && strings.HasPrefix(kv, strings.SplitN(marker, "=", 2)[0]+"=") {
				continue // explicit marker replaces any inherited value
			}
			env = append(env, kv)
		}
		env = append(env, envSweepHelperMode+"=sleep")
		if marker != "" {
			env = append(env, marker)
		}
		cmd := exec.Command(exe, "-test.run=^TestHelperEnvSweep$", "-test.timeout=140s")
		cmd.Env = env
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			fmt.Println("ERR:", err)
			return
		}
		fmt.Println("GRANDCHILD:", cmd.Process.Pid)
		_ = cmd.Process.Release()
	}
}

// scrubbedEnviron returns os.Environ() without any inherited kill-scope
// or helper-control entries, so fixtures only carry what a test set.
func scrubbedEnviron() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, KillScopeEnvKey+"=") ||
			strings.HasPrefix(kv, envSweepHelperMode+"=") ||
			strings.HasPrefix(kv, envSweepHelperEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func randomScope(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "test-sweep-" + hex.EncodeToString(b[:])
}

func skipUnlessSweepPlatform(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("env sweep unsupported on %s", runtime.GOOS)
	}
}

// spawnDetachedMarkedProcess re-execs the test binary so that a
// grandchild carrying key=value in its environment ends up reparented
// to PID 1, and returns that grandchild's PID.
func spawnDetachedMarkedProcess(t *testing.T, key, value string) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHelperEnvSweep$", "-test.v")
	cmd.Env = append(scrubbedEnviron(),
		envSweepHelperMode+"=orphan",
		envSweepHelperEnv+"="+key+"="+value)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("orphan helper: %v (output: %s)", err, out)
	}
	var pid int
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "GRANDCHILD: "); ok {
			pid, err = strconv.Atoi(rest)
			if err != nil {
				t.Fatalf("bad helper pid %q: %v", rest, err)
			}
		}
	}
	if pid == 0 {
		t.Fatalf("helper reported no grandchild PID (output: %s)", out)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
	// The helper (the grandchild's parent) has exited and been reaped by
	// cmd.Output, so the grandchild must reparent away from it; poll
	// until the kernel has done so before asserting anything about it.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			t.Fatalf("grandchild %d vanished before test began: %v", pid, err)
		}
		if strings.TrimSpace(string(out)) == "1" {
			return pid
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild %d did not reparent to PID 1", pid)
	return 0
}

func TestFindProcessesWithEnvValue_FindsDetachedOrphan(t *testing.T) {
	skipUnlessSweepPlatform(t)
	scope := randomScope(t)
	pid := spawnDetachedMarkedProcess(t, KillScopeEnvKey, scope)

	found := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !found {
		pids, err := findProcessesWithEnvValue(KillScopeEnvKey, scope)
		if err != nil {
			t.Fatalf("findProcessesWithEnvValue: %v", err)
		}
		for _, p := range pids {
			if p == pid {
				found = true
			}
		}
		if !found {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !found {
		t.Fatalf("detached PID %d carrying %s=%s not found by scanner", pid, KillScopeEnvKey, scope)
	}

	// A different scope value must not match the same process.
	other, err := findProcessesWithEnvValue(KillScopeEnvKey, scope+"x")
	if err != nil {
		t.Fatalf("findProcessesWithEnvValue(other): %v", err)
	}
	for _, p := range other {
		if p == pid {
			t.Fatalf("PID %d matched a scope value it does not carry", pid)
		}
	}
}

func TestSweepProcessesWithEnvValue_KillsDetachedOrphan(t *testing.T) {
	skipUnlessSweepPlatform(t)
	scope := randomScope(t)
	pid := spawnDetachedMarkedProcess(t, KillScopeEnvKey, scope)

	res, err := SweepProcessesWithEnvValue(KillScopeEnvKey, scope, 5*time.Second)
	if err != nil {
		t.Fatalf("SweepProcessesWithEnvValue: %v", err)
	}
	matched := false
	for _, p := range res.Matched {
		if p == pid {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("sweep did not match detached PID %d (matched: %v)", pid, res.Matched)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && pidAlive(pid) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatalf("detached PID %d still alive after sweep (survivors: %v)", pid, res.Survivors)
	}
}

// TestFindProcessesWithEnvValue_IgnoresArgvOnlyMatch pins the property
// that keeps the sweep from killing the tmux server: a process whose
// *argv* contains the token (but whose environment does not) is never a
// match. The fixture keeps a shell looping so it can't exec-optimize
// the token-bearing -c argument away.
func TestFindProcessesWithEnvValue_IgnoresArgvOnlyMatch(t *testing.T) {
	skipUnlessSweepPlatform(t)
	scope := randomScope(t)
	token := KillScopeEnvKey + "=" + scope

	cmd := exec.Command("sh", "-c", "while :; do sleep 1; done # "+token)
	cmd.Env = scrubbedEnviron()
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn argv fixture: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Guard against a vacuous pass: the token must actually be visible
	// in the process's argv before we assert the scanner ignores it.
	argvVisible := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !argvVisible {
		out, err := exec.Command("ps", "-xww", "-o", "pid=,command=").Output()
		if err != nil {
			t.Fatalf("ps: %v", err)
		}
		if line, ok := parsePSPidCommand(string(out))[pid]; ok && containsToken(line, token) {
			argvVisible = true
		} else {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !argvVisible {
		t.Fatalf("fixture PID %d never showed the token in argv — test would be vacuous", pid)
	}

	pids, err := findProcessesWithEnvValue(KillScopeEnvKey, scope)
	if err != nil {
		t.Fatalf("findProcessesWithEnvValue: %v", err)
	}
	for _, p := range pids {
		if p == pid {
			t.Fatalf("argv-only PID %d matched — the sweep would kill the tmux server", pid)
		}
	}
}

func TestSweepProcessesWithEnvValue_RejectsInvalidTokens(t *testing.T) {
	cases := []struct{ key, value string }{
		{"", "x"},
		{KillScopeEnvKey, ""},
		{"A B", "v"},
		{"KEY=", "v"},
		{KillScopeEnvKey, "with space"},
	}
	for _, c := range cases {
		if _, err := SweepProcessesWithEnvValue(c.key, c.value, time.Second); err == nil {
			t.Errorf("SweepProcessesWithEnvValue(%q, %q) accepted an invalid token", c.key, c.value)
		}
	}
}

func TestDiffEnvMatches_Fixtures(t *testing.T) {
	token := "AGENTDECK_KILL_SCOPE=abc.def0123"
	withEnv := strings.Join([]string{
		"  123 /usr/local/bin/node server.js FOO=bar " + token + " PATH=/usr/bin",
		"  456 tmux -L sock new-session -d -e " + token,
		"  555 /usr/local/bin/bun dev " + token,
		"  789 unrelated process",
		"garbage line without pid",
		"",
	}, "\n")
	argvOnly := strings.Join([]string{
		"  123 /usr/local/bin/node server.js",
		"  456 tmux -L sock new-session -d -e " + token,
		// 555 deliberately missing: appeared/exited between snapshots.
		"  789 unrelated process",
	}, "\n")

	got := diffEnvMatches(withEnv, argvOnly, token)
	if len(got) != 1 || got[0] != 123 {
		t.Fatalf("diffEnvMatches = %v, want [123]: 456 is an argv match (tmux server), 555 is missing from the argv snapshot", got)
	}
}

func TestContainsToken_Boundaries(t *testing.T) {
	token := "K=v1"
	cases := []struct {
		s    string
		want bool
	}{
		{"K=v1", true},
		{"a K=v1 b", true},
		{"a K=v1", true},
		{"K=v1 b", true},
		{"XK=v1", false},
		{"K=v12", false},
		{"K=v1x K=v1", true},
		{"", false},
	}
	for _, c := range cases {
		if got := containsToken(c.s, token); got != c.want {
			t.Errorf("containsToken(%q, %q) = %v, want %v", c.s, token, got, c.want)
		}
	}
}

func TestVersionSupportsNewSessionEnvFlag(t *testing.T) {
	cases := []struct {
		ver  string
		want bool
	}{
		{"2.9", false},
		{"3.0", false},
		{"3.1c", false},
		{"3.2", true},
		{"3.2a", true},
		{"3.7b", true},
		{"4.0", true},
		{"master", true},
		{"next", true},
		{"", false},
		{"garbage", false},
	}
	for _, c := range cases {
		if got := versionSupportsNewSessionEnvFlag(c.ver); got != c.want {
			t.Errorf("versionSupportsNewSessionEnvFlag(%q) = %v, want %v", c.ver, got, c.want)
		}
	}
}

// TestStartCommandSpec_InjectsKillScope pins that session creation
// carries the -e marker (on tmux >= 3.2) and that each launch gets a
// unique value — the property that makes late async sweeps unable to
// touch a restarted session.
func TestStartCommandSpec_InjectsKillScope(t *testing.T) {
	skipIfNoTmuxBinary(t)
	if !supportsNewSessionEnvFlag() {
		t.Skip("local tmux < 3.2: no -e support, injection intentionally disabled")
	}
	extractScope := func(args []string) string {
		for i, a := range args {
			if a == "-e" && i+1 < len(args) && strings.HasPrefix(args[i+1], KillScopeEnvKey+"=") {
				return strings.TrimPrefix(args[i+1], KillScopeEnvKey+"=")
			}
		}
		return ""
	}
	s := NewSession("agent-deck-killscope-spec", t.TempDir())
	_, args1 := s.startCommandSpec(t.TempDir(), "")
	_, args2 := s.startCommandSpec(t.TempDir(), "echo hi")
	scope1, scope2 := extractScope(args1), extractScope(args2)
	if scope1 == "" || scope2 == "" {
		t.Fatalf("new-session args missing -e %s=… injection: %v / %v", KillScopeEnvKey, args1, args2)
	}
	if scope1 == scope2 {
		t.Fatalf("kill scope must be unique per launch, got %q twice", scope1)
	}
}

// TestKillScope_EndToEnd_ReapsDaemonizedPaneChild is the full-loop
// regression test for the orphaned-nx-executor bug: a process
// daemonizes out of a real tmux pane (inheriting the injected marker),
// the session is killed, and the marker sweep reaps it.
func TestKillScope_EndToEnd_ReapsDaemonizedPaneChild(t *testing.T) {
	skipUnlessSweepPlatform(t)
	skipIfNoTmuxBinary(t)
	if !supportsNewSessionEnvFlag() {
		t.Skip("local tmux < 3.2: kill-scope pipeline disabled")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	pidFile := t.TempDir() + "/grandchild.pid"
	// The pane command re-execs this binary in orphan mode; the
	// grandchild inherits the pane environment — including the injected
	// AGENTDECK_KILL_SCOPE — exactly like a daemonizing dev server.
	paneCmd := fmt.Sprintf("%s=orphan %s -test.run='^TestHelperEnvSweep$' -test.v > %s; sleep 60",
		envSweepHelperMode, exe, pidFile)

	s := NewSession("agent-deck-killscope-e2e", t.TempDir())
	s.RunCommandAsInitialProcess = true
	if err := s.Start(paneCmd); err != nil {
		t.Skipf("could not start tmux session in this environment: %v", err)
	}
	t.Cleanup(func() { _ = s.KillAndWait() })

	scope, err := s.GetEnvironment(KillScopeEnvKey)
	if err != nil || scope == "" {
		t.Fatalf("kill scope not readable from live session (err: %v)", err)
	}

	// Wait for the helper to report the daemonized grandchild.
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && pid == 0 {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "GRANDCHILD: "); ok {
					pid, _ = strconv.Atoi(rest)
				}
			}
		}
		if pid == 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if pid == 0 {
		t.Fatalf("pane helper never reported a daemonized grandchild")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	if err := s.KillAndWait(); err != nil {
		t.Fatalf("KillAndWait: %v", err)
	}
	res, err := SweepProcessesWithEnvValue(KillScopeEnvKey, scope, 5*time.Second)
	if err != nil {
		t.Fatalf("SweepProcessesWithEnvValue: %v", err)
	}
	waitDead := time.Now().Add(2 * time.Second)
	for time.Now().Before(waitDead) && pidAlive(pid) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatalf("daemonized pane child %d survived kill + sweep (result: %+v) — orphaned-executor regression", pid, res)
	}
}
