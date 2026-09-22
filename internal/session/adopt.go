package session

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/takaaki-s/jind-ai/internal/tmux"
)

type AdoptOptions struct {
	Server          tmux.ServerRef
	Target          string
	AgentKind       string
	Description     string
	Fleet           string
	ConfirmationKey string
}

type AdoptionPreview struct {
	Server          tmux.ServerRef    `json:"server"`
	Pane            tmux.PaneInfo     `json:"pane"`
	Owner           *Info             `json:"owner,omitempty"`
	Detection       AgentDetection    `json:"detection"`
	Capabilities    AgentCapabilities `json:"capabilities"`
	ConfirmationKey string            `json:"confirmation_key"`
}

func adoptionIdentity(server tmux.ServerRef, pane tmux.PaneInfo) string {
	payload := fmt.Sprintf("v1\x00%s\x00%s\x00%s\x00%s\x00%d\x00%s",
		server.DisplayName(), pane.SessionID, pane.WindowID, pane.PaneID, pane.PanePID, pane.PaneStarted)
	sum := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("adopt-v1-%x", sum[:16])
}

func adoptionConfirmationKey(identity string, detection AgentDetection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "v1\x00%s\x00%s\x00%s", identity, detection.Provenance, detection.SelectedKind)
	for _, candidate := range detection.Candidates {
		fmt.Fprintf(&b, "\x00%s", candidate.Kind)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("adopt-confirm-v1-%x", sum[:16])
}

func (m *Manager) managedTmuxServer() tmux.ServerRef {
	name := m.tmuxSocketName
	if name == "" {
		name = tmux.DefaultSocketName()
	}
	return tmux.ServerRef{Kind: tmux.ServerName, Value: name}
}

func (m *Manager) runnerForSession(sess *Session) (tmux.Runner, error) {
	if !sess.IsAdopted() {
		if m.tmuxClient == nil {
			return nil, fmt.Errorf("tmux client not available")
		}
		return m.tmuxClient, nil
	}
	if m.tmuxFactory == nil {
		return nil, fmt.Errorf("tmux client factory not available")
	}
	return m.tmuxFactory(sess.TmuxBinding.Server)
}

func sameTmuxPane(server tmux.ServerRef, paneID string, sess *Session, managed tmux.ServerRef) bool {
	if paneID == "" || sess.TmuxPaneID != paneID {
		return false
	}
	otherServer := managed
	if sess.IsAdopted() {
		otherServer = sess.TmuxBinding.Server
	}
	return server == otherServer
}

// PreviewAdoption is read-only. It resolves target to one exact pane and
// reports any jind-ai record that already owns that server/pane pair.
func (m *Manager) PreviewAdoption(opts AdoptOptions) (AdoptionPreview, error) {
	if err := opts.Server.Validate(); err != nil {
		return AdoptionPreview{}, err
	}
	if strings.TrimSpace(opts.Target) == "" {
		return AdoptionPreview{}, fmt.Errorf("tmux pane target is required")
	}
	if m.tmuxFactory == nil {
		return AdoptionPreview{}, fmt.Errorf("tmux client factory not available")
	}
	tc, err := m.tmuxFactory(opts.Server)
	if err != nil {
		return AdoptionPreview{}, err
	}
	pane, err := tc.InspectPane(opts.Target)
	if err != nil {
		return AdoptionPreview{}, err
	}
	if pane.Dead {
		return AdoptionPreview{}, fmt.Errorf("tmux pane %s is dead and cannot be adopted", pane.PaneID)
	}
	detection, err := detectAgent(pane, m.agentResolver, opts.AgentKind)
	if err != nil {
		return AdoptionPreview{}, err
	}
	identity := adoptionIdentity(opts.Server, pane)
	key := adoptionConfirmationKey(identity, detection)
	baseCapabilities := AgentCapabilities{}
	if detection.SelectedKind != "" && detection.SelectedKind != GenericAgentKind {
		baseCapabilities = capabilitiesForKind(m.agentResolver, detection.SelectedKind)
	}
	preview := AdoptionPreview{
		Server: opts.Server, Pane: pane, Detection: detection, ConfirmationKey: key,
		Capabilities: adoptedCapabilities(baseCapabilities),
	}
	managed := m.managedTmuxServer()
	m.mu.RLock()
	for _, sess := range m.sessions {
		if sameTmuxPane(opts.Server, pane.PaneID, sess, managed) {
			info := sess.ToInfo()
			preview.Owner = &info
			break
		}
	}
	m.mu.RUnlock()
	return preview, nil
}

// AdoptPane repeats the inspection and commits only when it still matches the
// key printed by PreviewAdoption. No tmux mutation occurs on either path.
func (m *Manager) AdoptPane(opts AdoptOptions) (Info, error) {
	if opts.ConfirmationKey == "" {
		return Info{}, fmt.Errorf("confirmation key from dry-run is required")
	}
	m.adoptMu.Lock()
	defer m.adoptMu.Unlock()

	preview, err := m.PreviewAdoption(opts)
	if err != nil {
		return Info{}, err
	}
	if preview.ConfirmationKey != opts.ConfirmationKey {
		return Info{}, fmt.Errorf("tmux pane or agent selection changed after preview; run the dry-run again")
	}
	if preview.Detection.SelectedKind == "" {
		return Info{}, fmt.Errorf("agent detection is ambiguous; run the dry-run again with --agent <kind> or --agent generic")
	}
	if preview.Owner != nil {
		identity := adoptionIdentity(opts.Server, preview.Pane)
		if preview.Owner.TmuxBinding.IdentityKey == identity &&
			preview.Owner.AgentKind == preview.Detection.SelectedKind &&
			preview.Owner.AgentDetection.Provenance == preview.Detection.Provenance {
			return *preview.Owner, nil // idempotent retry of the same adoption
		}
		return Info{}, fmt.Errorf("tmux pane %s is already owned by session %s (%s)",
			preview.Pane.PaneID, preview.Owner.ID, preview.Owner.Description)
	}

	description := strings.TrimSpace(opts.Description)
	locked := description != ""
	if description == "" {
		description = fmt.Sprintf("%s @ %s", preview.Pane.CurrentCommand, preview.Pane.CanonicalTarget())
	}
	fleet := opts.Fleet
	if fleet == "" {
		fleet = DefaultFleet
	}
	now := time.Now()
	sess := &Session{
		ID: uuid.New().String(), Description: description, DescriptionLocked: locked,
		WorkDir: preview.Pane.CurrentPath, CurrentWorkDir: preview.Pane.CurrentPath,
		CreatedAt: now, LastActiveAt: now, Status: StatusRunning,
		AgentKind: preview.Detection.SelectedKind, AgentDetection: preview.Detection,
		Fleet: fleet, Capabilities: preview.Capabilities,
		TmuxWindowName: preview.Pane.SessionName, TmuxPaneID: preview.Pane.PaneID,
		TmuxBinding: TmuxBinding{
			Ownership: TmuxOwnershipAdopted, Server: opts.Server,
			SessionID: preview.Pane.SessionID, SessionName: preview.Pane.SessionName,
			WindowID: preview.Pane.WindowID, WindowName: preview.Pane.WindowName,
			WindowIndex: preview.Pane.WindowIndex, PaneID: preview.Pane.PaneID,
			PaneIndex: preview.Pane.PaneIndex, PanePID: preview.Pane.PanePID,
			PaneStarted: preview.Pane.PaneStarted, IdentityKey: adoptionIdentity(opts.Server, preview.Pane),
		},
		ReviewBase: ReviewBase{UnavailableReason: ReviewBaseUnavailableNotManagedWorktree},
		RepoName:   ResolveRepoName(preview.Pane.CurrentPath),
	}
	m.mu.Lock()
	m.sessions[sess.ID] = sess
	saved := *sess
	info := sess.ToInfo()
	if err := m.store.Save(saved); err != nil {
		delete(m.sessions, sess.ID)
		m.mu.Unlock()
		return Info{}, err
	}
	m.mu.Unlock()
	go m.captureOutputTmux(sess)
	return info, nil
}
