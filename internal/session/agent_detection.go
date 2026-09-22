package session

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/takaaki-s/jind-ai/internal/tmux"
)

// GenericAgentKind is the adoption-only kind used when no registered adapter
// matches a foreign pane. It is intentionally absent from the spawn registry:
// a generic record can be observed and manually inspected, but jin has no
// command, hook, transcript, or resume contract with which to create one.
const GenericAgentKind = "generic"

type AgentDetectionProvenance string

const (
	AgentDetectionDetected     AgentDetectionProvenance = "detected"
	AgentDetectionGeneric      AgentDetectionProvenance = "generic"
	AgentDetectionAmbiguous    AgentDetectionProvenance = "ambiguous"
	AgentDetectionUserSelected AgentDetectionProvenance = "user-selected"
)

type AgentDetectionEvidence struct {
	Source  string `json:"source"`
	PID     int    `json:"pid,omitempty"`
	Command string `json:"command"`
}

type AgentDetectionCandidate struct {
	Kind     string                   `json:"kind"`
	Score    int                      `json:"score"`
	Evidence []AgentDetectionEvidence `json:"evidence"`
}

// AgentDetection records classification separately from effective
// capabilities. A binary-name match can select an adapter label, but it never
// proves that jin installed that adapter's hooks or owns its resumable state.
type AgentDetection struct {
	Provenance   AgentDetectionProvenance  `json:"provenance"`
	SelectedKind string                    `json:"selected_kind,omitempty"`
	Candidates   []AgentDetectionCandidate `json:"candidates"`
}

func (d AgentDetection) IsZero() bool {
	return d.Provenance == "" && d.SelectedKind == "" && len(d.Candidates) == 0
}

const (
	detectionScoreCurrent = 400
	detectionScoreProcess = 300
	detectionScoreStart   = 200
	maxDetectionEvidence  = 8
)

func detectAgent(pane tmux.PaneInfo, resolver AgentResolver, requestedKind string) (AgentDetection, error) {
	candidates, catalogAvailable := detectAgentCandidates(pane, resolver)
	requestedKind = strings.TrimSpace(requestedKind)
	if requestedKind != "" {
		if requestedKind != GenericAgentKind {
			if resolver == nil {
				return AgentDetection{}, fmt.Errorf("agent registry is not available")
			}
			if _, err := resolver.Resolve(requestedKind); err != nil {
				return AgentDetection{}, err
			}
		}
		return AgentDetection{
			Provenance: AgentDetectionUserSelected, SelectedKind: requestedKind, Candidates: candidates,
		}, nil
	}
	if !catalogAvailable {
		return AgentDetection{}, fmt.Errorf("agent catalog is not available; select a kind explicitly")
	}
	switch len(candidates) {
	case 0:
		return AgentDetection{
			Provenance: AgentDetectionGeneric, SelectedKind: GenericAgentKind, Candidates: candidates,
		}, nil
	case 1:
		return AgentDetection{
			Provenance: AgentDetectionDetected, SelectedKind: candidates[0].Kind, Candidates: candidates,
		}, nil
	default:
		return AgentDetection{Provenance: AgentDetectionAmbiguous, Candidates: candidates}, nil
	}
}

func detectAgentCandidates(pane tmux.PaneInfo, resolver AgentResolver) ([]AgentDetectionCandidate, bool) {
	catalog, ok := resolver.(AgentCatalog)
	if !ok {
		return []AgentDetectionCandidate{}, false
	}
	agents := catalog.Agents()
	sort.SliceStable(agents, func(i, j int) bool { return agents[i].Kind() < agents[j].Kind() })

	candidates := make([]AgentDetectionCandidate, 0, len(agents))
	for _, ag := range agents {
		detector, ok := ag.(AgentExecutableDetector)
		if !ok {
			continue
		}
		candidate := AgentDetectionCandidate{Kind: ag.Kind(), Evidence: []AgentDetectionEvidence{}}
		seen := make(map[string]bool)
		add := func(score int, evidence AgentDetectionEvidence) {
			if evidence.Command == "" || !detector.RecognizesExecutable(evidence.Command) {
				return
			}
			key := fmt.Sprintf("%s\x00%d\x00%s", evidence.Source, evidence.PID, evidence.Command)
			if seen[key] {
				return
			}
			seen[key] = true
			if score > candidate.Score {
				candidate.Score = score
			}
			if len(candidate.Evidence) < maxDetectionEvidence {
				candidate.Evidence = append(candidate.Evidence, evidence)
			}
		}

		add(detectionScoreCurrent, AgentDetectionEvidence{
			Source: "pane-current-command", Command: executableName(pane.CurrentCommand),
		})
		add(detectionScoreStart, AgentDetectionEvidence{
			Source: "pane-start-command", Command: startCommandExecutable(pane.StartCommand),
		})
		for _, proc := range pane.ProcessAncestry {
			score := detectionScoreProcess
			if proc.PID == pane.PanePID {
				score += 20
			}
			add(score, AgentDetectionEvidence{
				Source: "process", PID: proc.PID, Command: executableName(proc.Command),
			})
		}
		if candidate.Score != 0 {
			candidates = append(candidates, candidate)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		return candidates[i].Kind < candidates[j].Kind
	})
	return candidates, true
}

func executableName(command string) string {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return ""
	}
	return filepath.Base(strings.Trim(fields[0], "'\""))
}

// startCommandExecutable handles only transparent launch modifiers. It does
// not search arbitrary arguments: treating `sh -c "echo claude"` as Claude
// Code would turn incidental text into ownership evidence.
func startCommandExecutable(command string) string {
	fields := strings.Fields(strings.TrimSpace(command))
	for len(fields) > 0 {
		token := strings.Trim(fields[0], "'\"")
		fields = fields[1:]
		switch token {
		case "exec", "command":
			continue
		case "env":
			for len(fields) > 0 {
				next := strings.Trim(fields[0], "'\"")
				if next == "-u" || next == "--unset" || next == "-C" || next == "--chdir" {
					fields = fields[1:]
					if len(fields) > 0 {
						fields = fields[1:]
					}
					continue
				}
				if strings.HasPrefix(next, "-") || strings.Contains(next, "=") {
					fields = fields[1:]
					continue
				}
				break
			}
			continue
		}
		if strings.Contains(token, "=") {
			continue
		}
		return filepath.Base(token)
	}
	return ""
}
