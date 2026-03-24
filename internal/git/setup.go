package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FindWorktreeSetupScript returns the path to the worktree setup script
// if one exists at <repoDir>/.agent-deck/worktree-setup.sh, or empty string.
func FindWorktreeSetupScript(repoDir string) string {
	p := filepath.Join(repoDir, ".agent-deck", "worktree-setup.sh")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// worktreeSetupTimeout is the maximum time a setup script is allowed to run.
var worktreeSetupTimeout = 60 * time.Second

// RunWorktreeSetupScript executes the setup script with AGENT_DECK_REPO_ROOT
// and AGENT_DECK_WORKTREE_PATH environment variables set. Working directory
// is set to worktreePath. Returns combined output and any error.
func RunWorktreeSetupScript(scriptPath, repoDir, worktreePath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), worktreeSetupTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-e", scriptPath)
	cmd.Dir = worktreePath
	cmd.Env = append(os.Environ(),
		"AGENT_DECK_REPO_ROOT="+repoDir,
		"AGENT_DECK_WORKTREE_PATH="+worktreePath,
	)
	cmd.WaitDelay = 5 * time.Second

	output, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(output))

	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("worktree setup script timed out after %s", worktreeSetupTimeout)
	}
	if err != nil {
		return out, fmt.Errorf("worktree setup script failed: %w", err)
	}
	return out, nil
}

// CreateWorktreeWithSetup creates a worktree and runs the setup script if present.
// Setup script failure is non-fatal: the worktree is still valid.
// Returns setup output, setup error (if any), and worktree creation error.
func CreateWorktreeWithSetup(repoDir, worktreePath, branchName string) (setupOutput string, setupErr error, err error) {
	if err = CreateWorktree(repoDir, worktreePath, branchName); err != nil {
		return "", nil, err
	}

	scriptPath := FindWorktreeSetupScript(repoDir)
	if scriptPath == "" {
		return "", nil, nil
	}

	setupOutput, setupErr = RunWorktreeSetupScript(scriptPath, repoDir, worktreePath)
	return setupOutput, setupErr, nil
}
