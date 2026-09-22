package tmux

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// ServerKind identifies how a local tmux server is addressed. Remote servers
// deliberately do not fit this type; adopting them is a separate transport
// problem rather than another spelling of a local socket.
type ServerKind string

const (
	ServerDefault ServerKind = "default"
	ServerName    ServerKind = "name"
	ServerPath    ServerKind = "path"
)

// ServerRef is the stable, persisted identity of a local tmux server.
type ServerRef struct {
	Kind  ServerKind `json:"kind"`
	Value string     `json:"value,omitempty"`
}

func (r ServerRef) Validate() error {
	switch r.Kind {
	case ServerDefault:
		if r.Value != "" {
			return fmt.Errorf("default tmux server must not have a value")
		}
	case ServerName, ServerPath:
		if strings.TrimSpace(r.Value) == "" {
			return fmt.Errorf("tmux server %s requires a value", r.Kind)
		}
	default:
		return fmt.Errorf("unknown tmux server kind %q", r.Kind)
	}
	return nil
}

// DisplayName is intentionally explicit: it is printed in adoption previews
// and must not make the default server look like jin's private named server.
func (r ServerRef) DisplayName() string {
	switch r.Kind {
	case ServerDefault:
		return "default"
	case ServerName:
		return "name:" + r.Value
	case ServerPath:
		return "path:" + r.Value
	default:
		return "unknown"
	}
}

// PaneProcess is one process in the pane leader's descendant tree. PPID is
// retained so callers can reconstruct branches without trusting display order.
type PaneProcess struct {
	PID     int    `json:"pid"`
	PPID    int    `json:"ppid"`
	Command string `json:"command"`
}

// PaneInfo is one atomic tmux snapshot plus a best-effort local process tree.
// PaneStarted and PanePID form the reuse-resistant identity used by adoption.
type PaneInfo struct {
	SessionID       string        `json:"session_id"`
	SessionName     string        `json:"session_name"`
	WindowID        string        `json:"window_id"`
	WindowName      string        `json:"window_name"`
	WindowIndex     int           `json:"window_index"`
	PaneID          string        `json:"pane_id"`
	PaneIndex       int           `json:"pane_index"`
	PanePID         int           `json:"pane_pid"`
	PaneStarted     string        `json:"pane_started"`
	CurrentCommand  string        `json:"current_command"`
	StartCommand    string        `json:"start_command,omitempty"`
	CurrentPath     string        `json:"current_path"`
	Dead            bool          `json:"dead"`
	ProcessAncestry []PaneProcess `json:"process_ancestry"`
}

func (p PaneInfo) CanonicalTarget() string {
	return fmt.Sprintf("%s:%d.%d", p.SessionName, p.WindowIndex, p.PaneIndex)
}

const paneInspectSeparator = "__jin_pane_field_6f12f2d6__"

// InspectPane resolves target once and returns its exact identity. Keeping all
// tmux fields in one display-message call prevents a pane move between probes
// from producing a synthetic identity assembled from two different panes.
func (c *Client) InspectPane(target string) (PaneInfo, error) {
	format := strings.Join([]string{
		"#{session_id}", "#{session_name}", "#{window_id}", "#{window_name}",
		"#{window_index}", "#{pane_id}", "#{pane_index}", "#{pane_pid}",
		"#{pane_current_command}", "#{pane_start_command}",
		"#{pane_current_path}", "#{pane_dead}",
	}, paneInspectSeparator)
	out, err := c.run("display-message", "-p", "-t", target, format)
	if err != nil {
		return PaneInfo{}, fmt.Errorf("inspect tmux pane %q: %w", target, err)
	}
	parts := strings.Split(out, paneInspectSeparator)
	if len(parts) != 12 {
		return PaneInfo{}, fmt.Errorf("inspect tmux pane %q: unexpected tmux response (%d fields): %q", target, len(parts), out)
	}
	windowIndex, err := strconv.Atoi(parts[4])
	if err != nil {
		return PaneInfo{}, fmt.Errorf("inspect tmux pane %q: invalid window index %q", target, parts[4])
	}
	paneIndex, err := strconv.Atoi(parts[6])
	if err != nil {
		return PaneInfo{}, fmt.Errorf("inspect tmux pane %q: invalid pane index %q", target, parts[6])
	}
	panePID, err := strconv.Atoi(parts[7])
	if err != nil || panePID <= 0 {
		return PaneInfo{}, fmt.Errorf("inspect tmux pane %q: invalid pane pid %q", target, parts[7])
	}
	dead, err := strconv.ParseBool(parts[11])
	if err != nil {
		return PaneInfo{}, fmt.Errorf("inspect tmux pane %q: invalid dead state %q", target, parts[11])
	}
	info := PaneInfo{
		SessionID: parts[0], SessionName: parts[1], WindowID: parts[2], WindowName: parts[3],
		WindowIndex: windowIndex, PaneID: parts[5], PaneIndex: paneIndex, PanePID: panePID,
		CurrentCommand: parts[8], StartCommand: parts[9], CurrentPath: parts[10], Dead: dead,
	}
	if info.SessionID == "" || info.SessionName == "" || info.WindowID == "" || info.PaneID == "" {
		return PaneInfo{}, fmt.Errorf("inspect tmux pane %q: tmux returned an incomplete identity", target)
	}
	info.ProcessAncestry, err = processDescendants(panePID)
	if err != nil {
		return PaneInfo{}, fmt.Errorf("inspect tmux pane %q process ancestry: %w", target, err)
	}
	info.PaneStarted, err = processStartTime(panePID)
	if err != nil {
		return PaneInfo{}, fmt.Errorf("inspect tmux pane %q process start time: %w", target, err)
	}
	return info, nil
}

var processListCommand = func() *exec.Cmd {
	return exec.Command("ps", "-eo", "pid=,ppid=,comm=")
}

var processStartCommand = func(pid int) *exec.Cmd {
	return exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
}

func processStartTime(pid int) (string, error) {
	out, err := processStartCommand(pid).Output()
	if err != nil {
		return "", err
	}
	started := strings.Join(strings.Fields(string(out)), " ")
	if started == "" {
		return "", fmt.Errorf("pane pid %d is no longer present", pid)
	}
	return started, nil
}

// processDescendants returns the pane leader and all of its descendants in a
// deterministic parent-before-child order. It is named ancestry in the wire
// shape because every row retains its parent link; unlike a single "current
// command", this exposes wrappers, shells and the actual agent process.
func processDescendants(rootPID int) ([]PaneProcess, error) {
	out, err := processListCommand().Output()
	if err != nil {
		return nil, err
	}
	return parseProcessDescendants(out, rootPID)
}

func parseProcessDescendants(out []byte, rootPID int) ([]PaneProcess, error) {
	all := make(map[int]PaneProcess)
	children := make(map[int][]int)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		all[pid] = PaneProcess{PID: pid, PPID: ppid, Command: strings.Join(fields[2:], " ")}
		children[ppid] = append(children[ppid], pid)
	}
	if _, ok := all[rootPID]; !ok {
		return nil, fmt.Errorf("pane pid %d is no longer present", rootPID)
	}
	for ppid := range children {
		sort.Ints(children[ppid])
	}
	result := make([]PaneProcess, 0, 4)
	queue := []int{rootPID}
	seen := make(map[int]bool)
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if proc, ok := all[pid]; ok {
			result = append(result, proc)
			queue = append(queue, children[pid]...)
		}
	}
	return result, nil
}
