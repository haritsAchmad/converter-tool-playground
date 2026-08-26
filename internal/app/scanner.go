package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

var errMalwareDetected = errors.New("malware detected")

type malwareScanner interface {
	Scan(context.Context, string) error
}

type clamScanner struct {
	path string
}

func newClamScanner(configuredPath string) (malwareScanner, error) {
	if configuredPath == "" {
		return nil, nil
	}
	path, err := exec.LookPath(configuredPath)
	if err != nil {
		return nil, fmt.Errorf("configured clamscan executable not found: %w", err)
	}
	return &clamScanner{path: path}, nil
}

func (s *clamScanner) Scan(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, s.path, "--no-summary", path)
	cmd.Env = []string{}
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return errMalwareDetected
	}
	if ctx.Err() != nil {
		return fmt.Errorf("clamscan timed out: %w", ctx.Err())
	}
	return fmt.Errorf("clamscan failed: %w (%s)", err, output)
}
