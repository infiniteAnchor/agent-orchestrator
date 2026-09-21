package tasksched

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"
)

const verifyTimeout = 30 * time.Second

// ExecVerifier runs verification commands in the project directory. Exit codes
// are the evidence. Command output is discarded so a host path printed by a
// test cannot become part of the durable result.
type ExecVerifier struct{}

type verifyEvidence struct {
	Passed bool         `json:"passed"`
	Steps  []verifyStep `json:"steps"`
}

type verifyStep struct {
	ExitCode int `json:"exitCode"`
}

// Verify returns inconclusive when the directory or the command cannot be
// started. A non-zero exit is a failed verification, not an unknown probe.
func (ExecVerifier) Verify(ctx context.Context, dir string, commands []string) (VerifyReport, error) {
	if dir == "" {
		return inconclusive("project directory is unavailable"), nil
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return inconclusive("project directory is unavailable"), nil
	}
	steps := make([]verifyStep, 0, len(commands))
	for _, command := range commands {
		exitCode, startErr := runCommand(ctx, dir, command)
		if startErr != nil {
			return inconclusive("verification command could not start"), nil
		}
		steps = append(steps, verifyStep{ExitCode: exitCode})
		if exitCode != 0 {
			evidence, err := json.Marshal(verifyEvidence{Passed: false, Steps: steps})
			if err != nil {
				return VerifyReport{}, err
			}
			return VerifyReport{Passed: false, Evidence: evidence, Summary: "verification failed"}, nil
		}
	}
	evidence, err := json.Marshal(verifyEvidence{Passed: true, Steps: steps})
	if err != nil {
		return VerifyReport{}, err
	}
	return VerifyReport{Passed: true, Evidence: evidence, Summary: "verification passed"}, nil
}

func inconclusive(summary string) VerifyReport {
	evidence, _ := json.Marshal(verifyEvidence{Passed: false, Steps: []verifyStep{}})
	return VerifyReport{Inconclusive: true, Evidence: evidence, Summary: summary}
}

func runCommand(ctx context.Context, dir, command string) (int, error) {
	runCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(runCtx, "cmd", "/c", command)
	} else {
		cmd = exec.CommandContext(runCtx, "sh", "-c", command)
	}
	cmd.Dir = dir
	// Output is not evidence. Discard it so a noisy command cannot retain a
	// host path or grow without a bound until the timeout fires.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}
