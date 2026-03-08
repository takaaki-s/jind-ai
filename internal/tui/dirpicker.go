package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/takaaki-s/claude-code-valet/internal/config"
	"github.com/takaaki-s/claude-code-valet/internal/host"
)

// --- Async message types for remote SSH operations ---

type remoteHomeMsg struct {
	home string
	err  error
}

type remoteEntriesMsg struct {
	entries []string
	dir     string // リクエスト元ディレクトリ（stale検出用）
	err     error
}

type remoteDirExistsMsg struct {
	path   string
	exists bool
}

type spinnerTickMsg struct{}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func spinnerTickCmd() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(t time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

// --- History ---

// HistoryEntry represents a recently used directory for quick-select.
type HistoryEntry struct {
	Path        string    // フルパスまたは ~/... 形式
	DisplayPath string    // 表示用パス（~ 展開済み）
	LastUsedAt  time.Time // 最終利用日時
	HostID      string    // ホストID（"local" またはリモートホストID）
}

// DirHistoryRemoveMsg is emitted when a history entry should be removed from persistent storage.
type DirHistoryRemoveMsg struct {
	HostID string
	Path   string
}

// DirPickerModel is a directory browser component for selecting a working directory.
type DirPickerModel struct {
	currentDir string   // 現在表示中のディレクトリ
	entries    []string // 現在のディレクトリ内のサブディレクトリ名
	filtered   []string // フィルタ後のエントリ
	cursor     int      // カーソル位置（履歴+ディレクトリの統合リスト上）
	offset     int      // スクロールオフセット

	filterInput textinput.Model // フィルタ入力
	showHidden  bool            // 隠しディレクトリを表示するか

	selected bool   // ディレクトリが選択されたか
	result   string // 選択されたディレクトリパス

	width  int
	height int

	// Remote host support
	hostConfig *config.HostConfig // nil = local mode
	remoteHome string             // リモートのホームディレクトリ

	// Loading state (remote)
	loading     bool   // SSH操作実行中
	loadingDir  string // ロード中のディレクトリ（stale検出用）
	spinnerTick int    // スピナーフレームインデックス

	// History (recently used directories)
	historyDirs     []HistoryEntry // 履歴エントリ
	filteredHistory []HistoryEntry // フィルタ後の履歴エントリ
}

// NewDirPickerModel creates a new directory picker starting at the given path.
// If startDir is empty, defaults to the user's home directory.
func NewDirPickerModel(startDir string) DirPickerModel {
	if startDir == "" {
		startDir, _ = os.UserHomeDir()
	}
	// Expand ~
	if strings.HasPrefix(startDir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			startDir = filepath.Join(home, startDir[2:])
		}
	}
	// Ensure absolute path
	if !filepath.IsAbs(startDir) {
		if abs, err := filepath.Abs(startDir); err == nil {
			startDir = abs
		}
	}

	fi := textinput.New()
	fi.Placeholder = "type to filter..."
	fi.CharLimit = 256
	fi.Width = 40
	fi.Focus()

	m := DirPickerModel{
		currentDir:  startDir,
		filterInput: fi,
	}
	m.loadEntries()
	return m
}

// SetRemoteHost switches the directory picker to browse a remote host's filesystem via SSH.
// Returns a tea.Cmd to asynchronously fetch the remote home directory.
func (m *DirPickerModel) SetRemoteHost(hc *config.HostConfig) tea.Cmd {
	if hc == nil || hc.Type != "ssh" {
		m.hostConfig = nil
		return nil
	}
	m.hostConfig = hc
	m.currentDir = "/home" // 仮のデフォルト
	m.cursor = 0
	m.offset = 0
	m.entries = nil
	m.filtered = nil
	m.loading = true
	m.loadingDir = ""

	hostConfig := *hc // クロージャ用コピー
	return tea.Batch(
		func() tea.Msg {
			home, err := getRemoteHome(&hostConfig)
			return remoteHomeMsg{home: home, err: err}
		},
		spinnerTickCmd(),
	)
}

// ClearRemoteHost switches back to local directory browsing.
func (m *DirPickerModel) ClearRemoteHost() {
	m.hostConfig = nil
	m.remoteHome = ""
	m.loading = false
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "/"
	}
	m.currentDir = home
	m.cursor = 0
	m.offset = 0
	m.loadEntries()
}

// SetHistory sets the directory history entries for quick-select.
// Entries should be pre-sorted by LastUsedAt (most recent first).
func (m *DirPickerModel) SetHistory(entries []HistoryEntry) {
	m.historyDirs = entries
	m.updateFilteredHistory()
}

// IsRemote returns true if the directory picker is browsing a remote host.
func (m *DirPickerModel) IsRemote() bool {
	return m.hostConfig != nil
}

// Selected returns true if a directory was selected.
func (m *DirPickerModel) Selected() bool {
	return m.selected
}

// Result returns the selected directory path.
func (m *DirPickerModel) Result() string {
	return m.result
}

// Init implements tea.Model.
func (m DirPickerModel) Init() tea.Cmd {
	return textinput.Blink
}

// totalItems returns the total number of selectable items (history + directories).
func (m DirPickerModel) totalItems() int {
	return len(m.filteredHistory) + len(m.filtered)
}

// Update implements tea.Model.
func (m DirPickerModel) Update(msg tea.Msg) (DirPickerModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	// --- Async SSH responses ---

	case remoteHomeMsg:
		if m.hostConfig == nil {
			return m, nil // stale
		}
		if msg.err != nil || msg.home == "" {
			m.remoteHome = "/home"
		} else {
			m.remoteHome = msg.home
		}
		m.currentDir = m.remoteHome
		m.loading = true
		m.loadingDir = m.currentDir
		hc := *m.hostConfig
		dir := m.currentDir
		showHidden := m.showHidden
		return m, tea.Batch(
			func() tea.Msg {
				entries, err := listRemoteDirectories(&hc, dir, showHidden)
				return remoteEntriesMsg{entries: entries, dir: dir, err: err}
			},
			spinnerTickCmd(),
		)

	case remoteEntriesMsg:
		// stale応答を無視
		if msg.dir != m.currentDir {
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.entries = nil
			m.filtered = nil
			return m, nil
		}
		m.entries = msg.entries
		sort.Strings(m.entries)
		m.applyFilter()
		return m, nil

	case remoteDirExistsMsg:
		if !msg.exists {
			return m, nil
		}
		m.currentDir = msg.path
		m.filterInput.SetValue("")
		m.cursor = 0
		m.offset = 0
		return m, m.loadRemoteEntriesCmd()

	case spinnerTickMsg:
		if m.loading {
			m.spinnerTick = (m.spinnerTick + 1) % len(spinnerFrames)
			return m, spinnerTickCmd()
		}
		return m, nil

	// --- Key input ---

	case tea.KeyMsg:
		// loading中はナビゲーションキーのみ許可
		if m.loading {
			return m, nil
		}

		historyLen := len(m.filteredHistory)

		switch msg.String() {
		case "enter":
			if m.totalItems() > 0 && m.cursor < m.totalItems() {
				if m.cursor < historyLen {
					// 履歴エントリ選択: 即座にそのパスを返す
					entry := m.filteredHistory[m.cursor]

					// ローカルの場合: ディレクトリ存在チェック
					if !m.IsRemote() {
						checkPath := entry.Path
						if strings.HasPrefix(checkPath, "~/") {
							if home, err := os.UserHomeDir(); err == nil {
								checkPath = filepath.Join(home, checkPath[2:])
							}
						}
						if info, err := os.Stat(checkPath); err != nil || !info.IsDir() {
							// 存在しない: 表示リストから除去 + 削除メッセージを返す
							m.removeFilteredHistoryEntry(m.cursor)
							return m, func() tea.Msg {
								return DirHistoryRemoveMsg{
									HostID: entry.HostID,
									Path:   entry.Path,
								}
							}
						}
					}

					m.selected = true
					m.result = entry.Path
					return m, nil
				}
				// 通常ディレクトリに入る
				dirIdx := m.cursor - historyLen
				if dirIdx < len(m.filtered) {
					selected := m.filtered[dirIdx]
					m.currentDir = filepath.Join(m.currentDir, selected)
					m.filterInput.SetValue("")
					m.cursor = 0
					m.offset = 0
					if m.IsRemote() {
						return m, m.loadRemoteEntriesCmd()
					}
					m.loadEntries()
				}
			}
			return m, nil

		case "tab", "ctrl+d":
			// Select current directory
			m.selected = true
			if m.IsRemote() {
				// リモートの場合: homeプレフィックスを ~ に変換して返す
				if m.remoteHome != "" && strings.HasPrefix(m.currentDir, m.remoteHome) {
					m.result = "~" + m.currentDir[len(m.remoteHome):]
					if m.result == "~" {
						m.result = "~"
					}
				} else {
					m.result = m.currentDir
				}
			} else {
				m.result = m.currentDir
			}
			return m, nil

		case "backspace":
			// If filter is empty, go to parent
			if m.filterInput.Value() == "" {
				parent := filepath.Dir(m.currentDir)
				if parent != m.currentDir {
					m.currentDir = parent
					m.cursor = 0
					m.offset = 0
					if m.IsRemote() {
						return m, m.loadRemoteEntriesCmd()
					}
					m.loadEntries()
				}
				return m, nil
			}

		case "up":
			if m.cursor > 0 {
				m.cursor--
				m.adjustScroll()
			}
			return m, nil

		case "down":
			if m.cursor < m.totalItems()-1 {
				m.cursor++
				m.adjustScroll()
			}
			return m, nil

		case "ctrl+h":
			// Toggle hidden directories
			m.showHidden = !m.showHidden
			if m.IsRemote() {
				return m, m.loadRemoteEntriesCmd()
			}
			m.loadEntries()
			return m, nil
		}
	}

	// Update filter input
	oldQuery := m.filterInput.Value()
	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)

	// Check for direct path navigation
	val := m.filterInput.Value()
	if val != oldQuery {
		if strings.HasPrefix(val, "/") || strings.HasPrefix(val, "~") {
			// Direct path navigation
			path := val
			if m.IsRemote() {
				// リモート: ~ をリモートhomeに展開
				if strings.HasPrefix(path, "~/") && m.remoteHome != "" {
					path = filepath.Join(m.remoteHome, path[2:])
				} else if path == "~" && m.remoteHome != "" {
					path = m.remoteHome
				}
				// リモートでディレクトリ存在チェック（非同期）
				hc := *m.hostConfig
				checkPath := path
				return m, tea.Batch(cmd, func() tea.Msg {
					exists := remoteDirExists(&hc, checkPath)
					return remoteDirExistsMsg{path: checkPath, exists: exists}
				})
			} else {
				// ローカル
				if strings.HasPrefix(path, "~/") {
					if home, err := os.UserHomeDir(); err == nil {
						path = filepath.Join(home, path[2:])
					}
				}
				if info, err := os.Stat(path); err == nil && info.IsDir() {
					m.currentDir = path
					m.filterInput.SetValue("")
					m.cursor = 0
					m.offset = 0
					m.loadEntries()
					return m, cmd
				}
			}
		}
		// Normal filtering
		m.applyFilter()
		m.updateFilteredHistory()
	}

	return m, cmd
}

// View renders the directory picker.
func (m DirPickerModel) View() string {
	var b strings.Builder

	// Breadcrumb: current path
	pathStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7aa2f7"))
	displayPath := m.currentDir
	if m.IsRemote() {
		// リモート: homeプレフィックスを ~ に変換
		if m.remoteHome != "" && strings.HasPrefix(displayPath, m.remoteHome) {
			displayPath = "~" + displayPath[len(m.remoteHome):]
		}
	} else {
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(displayPath, home) {
			displayPath = "~" + displayPath[len(home):]
		}
	}
	b.WriteString(pathStyle.Render("  📂 " + displayPath))
	b.WriteString("\n")

	// Filter input
	b.WriteString("  " + m.filterInput.View())
	b.WriteString("\n")

	// Separator
	sepWidth := m.width - 4
	if sepWidth < 10 {
		sepWidth = 40
	}

	// Calculate visible lines
	visibleLines := m.height - 8
	if visibleLines < 3 {
		visibleLines = 10
	}

	dirStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7dcfff"))
	historyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#bb9af7"))
	selectedStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("255")).
		Background(lipgloss.Color("#7aa2f7"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#414868"))
	sectionStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#565f89"))

	// Loading state
	if m.loading {
		b.WriteString("  " + strings.Repeat("─", sepWidth))
		b.WriteString("\n")
		spinner := spinnerFrames[m.spinnerTick]
		b.WriteString("  " + spinner + " Loading...")
		b.WriteString("\n")
	} else {
		historyLen := len(m.filteredHistory)

		// History section
		if historyLen > 0 {
			b.WriteString("  " + sectionStyle.Render("── Recently Used "+strings.Repeat("─", sepWidth-18)))
			b.WriteString("\n")
			for i, entry := range m.filteredHistory {
				if i == m.cursor {
					padded := "▸ " + entry.DisplayPath
					availWidth := m.width - 4
					if availWidth > 0 && len(padded) < availWidth {
						padded += strings.Repeat(" ", availWidth-len(padded))
					}
					b.WriteString("  " + selectedStyle.Render(padded))
				} else {
					b.WriteString("    " + historyStyle.Render(entry.DisplayPath))
				}
				b.WriteString("\n")
			}
		}

		// Directories section
		b.WriteString("  " + sectionStyle.Render("── Directories "+strings.Repeat("─", sepWidth-16)))
		b.WriteString("\n")

		if len(m.filtered) == 0 {
			b.WriteString("  " + dimStyle.Render("(empty)"))
			b.WriteString("\n")
		} else {
			// スクロール計算（履歴セクションの高さを考慮）
			// historyHeight = historyLen (entries) + 1 (header) if historyLen > 0
			historyHeight := 0
			if historyLen > 0 {
				historyHeight = historyLen + 1
			}
			dirVisibleLines := visibleLines - historyHeight - 1 // -1 for "Directories" header
			if dirVisibleLines < 3 {
				dirVisibleLines = 3
			}

			// ディレクトリ部分のスクロールオフセット計算
			dirOffset := 0
			if m.cursor >= historyLen {
				dirCursor := m.cursor - historyLen
				if dirCursor >= dirOffset+dirVisibleLines {
					dirOffset = dirCursor - dirVisibleLines + 1
				}
				if dirCursor < dirOffset {
					dirOffset = dirCursor
				}
			}

			end := dirOffset + dirVisibleLines
			if end > len(m.filtered) {
				end = len(m.filtered)
			}
			for i := dirOffset; i < end; i++ {
				entry := m.filtered[i]
				displayName := entry + "/"
				globalIdx := historyLen + i

				if globalIdx == m.cursor {
					// Pad selected line to full width
					padded := "▸ " + displayName
					availWidth := m.width - 4
					if availWidth > 0 && len(padded) < availWidth {
						padded += strings.Repeat(" ", availWidth-len(padded))
					}
					b.WriteString("  " + selectedStyle.Render(padded))
				} else {
					b.WriteString("    " + dirStyle.Render(displayName))
				}
				b.WriteString("\n")
			}
		}
	}

	// Footer hints
	b.WriteString("\n")
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	hiddenHint := "Ctrl+H:show hidden"
	if m.showHidden {
		hiddenHint = "Ctrl+H:hide hidden"
	}
	b.WriteString("  " + hintStyle.Render("Enter:open  Tab:select  Backspace:parent  "+hiddenHint))

	return b.String()
}

// --- Internal ---

func (m *DirPickerModel) loadEntries() {
	if m.IsRemote() {
		m.loadRemoteEntries()
	} else {
		m.loadLocalEntries()
	}
}

func (m *DirPickerModel) loadLocalEntries() {
	m.entries = nil
	m.filtered = nil

	dirEntries, err := os.ReadDir(m.currentDir)
	if err != nil {
		return
	}

	for _, entry := range dirEntries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !m.showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		m.entries = append(m.entries, name)
	}

	sort.Strings(m.entries)
	m.applyFilter()
}

func (m *DirPickerModel) loadRemoteEntries() {
	m.entries = nil
	m.filtered = nil

	if m.hostConfig == nil {
		return
	}

	entries, err := listRemoteDirectories(m.hostConfig, m.currentDir, m.showHidden)
	if err != nil {
		return
	}

	m.entries = entries
	sort.Strings(m.entries)
	m.applyFilter()
}

// loadRemoteEntriesCmd returns a tea.Cmd for async remote directory listing.
func (m *DirPickerModel) loadRemoteEntriesCmd() tea.Cmd {
	if m.hostConfig == nil {
		return nil
	}
	m.loading = true
	m.loadingDir = m.currentDir
	m.entries = nil
	m.filtered = nil

	hc := *m.hostConfig
	dir := m.currentDir
	showHidden := m.showHidden
	return tea.Batch(
		func() tea.Msg {
			entries, err := listRemoteDirectories(&hc, dir, showHidden)
			return remoteEntriesMsg{entries: entries, dir: dir, err: err}
		},
		spinnerTickCmd(),
	)
}

func (m *DirPickerModel) applyFilter() {
	query := strings.ToLower(m.filterInput.Value())
	if query == "" || strings.HasPrefix(query, "/") || strings.HasPrefix(query, "~") {
		m.filtered = m.entries
	} else {
		m.filtered = nil
		for _, e := range m.entries {
			if strings.Contains(strings.ToLower(e), query) {
				m.filtered = append(m.filtered, e)
			}
		}
	}
	if m.cursor >= m.totalItems() {
		m.cursor = 0
	}
	m.offset = 0
}

func (m *DirPickerModel) updateFilteredHistory() {
	if len(m.historyDirs) == 0 {
		m.filteredHistory = nil
		return
	}
	query := strings.ToLower(m.filterInput.Value())
	if query == "" || strings.HasPrefix(query, "/") || strings.HasPrefix(query, "~") {
		m.filteredHistory = m.historyDirs
	} else {
		m.filteredHistory = nil
		for _, h := range m.historyDirs {
			if strings.Contains(strings.ToLower(h.DisplayPath), query) {
				m.filteredHistory = append(m.filteredHistory, h)
			}
		}
	}
	if m.cursor >= m.totalItems() {
		m.cursor = 0
	}
}

// removeFilteredHistoryEntry removes a history entry at the given index from
// both filteredHistory and historyDirs, adjusting the cursor as needed.
func (m *DirPickerModel) removeFilteredHistoryEntry(idx int) {
	if idx < 0 || idx >= len(m.filteredHistory) {
		return
	}
	removed := m.filteredHistory[idx]
	m.filteredHistory = append(m.filteredHistory[:idx], m.filteredHistory[idx+1:]...)

	// historyDirsからも除去
	for i, h := range m.historyDirs {
		if h.Path == removed.Path && h.HostID == removed.HostID {
			m.historyDirs = append(m.historyDirs[:i], m.historyDirs[i+1:]...)
			break
		}
	}

	if m.cursor >= m.totalItems() && m.cursor > 0 {
		m.cursor--
	}
}

func (m *DirPickerModel) adjustScroll() {
	visibleLines := m.height - 8
	if visibleLines < 3 {
		visibleLines = 10
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+visibleLines {
		m.offset = m.cursor - visibleLines + 1
	}
}

// --- SSH remote helpers ---

// sshArgs returns common SSH arguments with ControlMaster multiplexing support.
func sshArgs(hc *config.HostConfig) []string {
	ctrlPath := host.SSHControlPath(hc.ID)
	args := []string{
		"-o", "ControlMaster=no",
		"-o", "ControlPath=" + ctrlPath,
		"-o", "ClearAllForwardings=yes",
	}
	args = append(args, hc.SSHOpts...)
	return args
}

// getRemoteHome gets the remote user's home directory via SSH.
func getRemoteHome(hc *config.HostConfig) (string, error) {
	args := sshArgs(hc)
	args = append(args, hc.Host, "echo $HOME")

	cmd := exec.Command("ssh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// listRemoteDirectories lists subdirectories of the given path on a remote host via SSH.
func listRemoteDirectories(hc *config.HostConfig, remotePath string, showHidden bool) ([]string, error) {
	// Use ls to list directories (compatible with most systems)
	var remoteCmd string
	if !showHidden {
		remoteCmd = "ls -1 -p " + remotePath + " 2>/dev/null | grep '/$' | grep -v '^\\..*/$' | sed 's|/$||'"
	} else {
		remoteCmd = "ls -1 -a -p " + remotePath + " 2>/dev/null | grep '/$' | grep -v '^\\.\\.\\?/$' | sed 's|/$||'"
	}

	args := sshArgs(hc)
	args = append(args, hc.Host, remoteCmd)

	cmd := exec.Command("ssh", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	output := strings.TrimSpace(string(out))
	if output == "" {
		return nil, nil
	}
	return strings.Split(output, "\n"), nil
}

// remoteDirExists checks if a directory exists on a remote host via SSH.
func remoteDirExists(hc *config.HostConfig, remotePath string) bool {
	if hc == nil {
		return false
	}
	args := sshArgs(hc)
	args = append(args, hc.Host, "test -d "+remotePath)

	cmd := exec.Command("ssh", args...)
	return cmd.Run() == nil
}
