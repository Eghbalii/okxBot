package api

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// UnitStatus is one supervised process's liveness/uptime/crash-count summary (CLAUDE.md §11.2).
// Deliberately sourced from the process supervisor (systemd/docker), not an in-process heartbeat
// — a crashed process can't reliably report its own crash.
type UnitStatus struct {
	Unit         string    `json:"unit"`
	Active       bool      `json:"active"`
	State        string    `json:"state"`    // e.g. "active", "failed", "inactive"
	SubState     string    `json:"subState"` // e.g. "running", "dead"
	StartedAt    time.Time `json:"startedAt,omitempty"`
	UptimeSec    int64     `json:"uptimeSec,omitempty"`
	RestartCount int       `json:"restartCount"`
	Error        string    `json:"error,omitempty"` // set if status couldn't be determined
}

// ProcessStatus queries the configured process manager ("systemd" or "docker") for a unit's
// current status. Shells out rather than linking a client library — both tools are simple,
// well-specified CLIs and this avoids a heavier dependency for a handful of fields.
func ProcessStatus(ctx context.Context, manager, unit string) UnitStatus {
	switch manager {
	case "docker":
		return dockerStatus(ctx, unit)
	default:
		return systemdStatus(ctx, unit)
	}
}

func systemdStatus(ctx context.Context, unit string) UnitStatus {
	status := UnitStatus{Unit: unit}
	out, err := runCommand(ctx, "systemctl", "show", unit,
		"--property=ActiveState,SubState,ExecMainStartTimestamp,NRestarts")
	if err != nil {
		status.Error = fmt.Sprintf("systemctl show %s: %v", unit, err)
		return status
	}

	fields := parseKeyEquals(out)
	status.State = fields["ActiveState"]
	status.SubState = fields["SubState"]
	status.Active = status.State == "active"
	if n, err := strconv.Atoi(fields["NRestarts"]); err == nil {
		status.RestartCount = n
	}
	if ts := fields["ExecMainStartTimestamp"]; ts != "" && ts != "n/a" {
		// systemd's default timestamp format, e.g. "Tue 2026-08-25 10:00:00 UTC".
		if t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", ts); err == nil {
			status.StartedAt = t
			status.UptimeSec = int64(time.Since(t).Seconds())
		}
	}
	return status
}

func dockerStatus(ctx context.Context, container string) UnitStatus {
	status := UnitStatus{Unit: container}
	out, err := runCommand(ctx, "docker", "inspect",
		"--format", "{{.State.Status}}|{{.State.StartedAt}}|{{.RestartCount}}", container)
	if err != nil {
		status.Error = fmt.Sprintf("docker inspect %s: %v", container, err)
		return status
	}

	parts := strings.SplitN(strings.TrimSpace(out), "|", 3)
	if len(parts) != 3 {
		status.Error = fmt.Sprintf("docker inspect %s: unexpected output %q", container, out)
		return status
	}
	status.State = parts[0]
	status.Active = parts[0] == "running"
	if t, err := time.Parse(time.RFC3339Nano, parts[1]); err == nil {
		status.StartedAt = t
		status.UptimeSec = int64(time.Since(t).Seconds())
	}
	if n, err := strconv.Atoi(parts[2]); err == nil {
		status.RestartCount = n
	}
	return status
}

// LogTail returns the last n log lines for a unit, filtered to error-level lines if errorsOnly.
func LogTail(ctx context.Context, manager, unit string, n int, errorsOnly bool) (string, error) {
	switch manager {
	case "docker":
		return runCommand(ctx, "docker", "logs", "--tail", strconv.Itoa(n), unit)
	default:
		args := []string{"-u", unit, "-n", strconv.Itoa(n), "--no-pager"}
		if errorsOnly {
			args = append(args, "-p", "err")
		}
		return runCommand(ctx, "journalctl", args...)
	}
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w (%s)", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func parseKeyEquals(out string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		fields[k] = v
	}
	return fields
}
