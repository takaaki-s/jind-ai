// Package transcript provides reading functionality for Claude Code transcript files.
package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Message represents a message from the transcript
type Message struct {
	Type      string // "user" or "assistant"
	Content   string // text content
	Timestamp string // ISO8601 timestamp
}

// LastMessages holds the last user and assistant messages
type LastMessages struct {
	User      *Message
	Assistant *Message
}

// Entry is a structured representation of a single transcript line.
// Unlike Message (display-oriented), Entry preserves all block kinds
// (text, thinking, tool_use, tool_result) and usage info, suitable for
// programmatic orchestration.
type Entry struct {
	Type      string  `json:"type"`                // "user" | "assistant" | "system" | ...
	Timestamp string  `json:"timestamp,omitempty"` // ISO8601
	Blocks    []Block `json:"blocks,omitempty"`
	Usage     *Usage  `json:"usage,omitempty"` // assistant only
}

// Block is a single content block within a transcript entry.
type Block struct {
	Kind      string          `json:"kind"`                  // "text" | "thinking" | "tool_use" | "tool_result"
	Text      string          `json:"text,omitempty"`        // text/thinking
	ToolName  string          `json:"tool_name,omitempty"`   // tool_use only (tool_result carries only id)
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_use id, or tool_result's referenced id
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use input (preserved structure)
	Output    string          `json:"output,omitempty"`      // tool_result content (string-ified)
	IsError   bool            `json:"is_error,omitempty"`    // tool_result error flag
}

// Usage captures Anthropic API usage info from an assistant message.
type Usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// Reader reads Claude Code transcript files
type Reader struct {
	claudeDir string
}

// NewReader creates a new transcript reader
func NewReader() *Reader {
	home, _ := os.UserHomeDir()
	return &Reader{
		claudeDir: filepath.Join(home, ".claude"),
	}
}

// GetLastMessage returns the last user or assistant message from the transcript
// workDir: the working directory of the session
// sessionID: the Claude Code session ID (UUID format)
func (r *Reader) GetLastMessage(workDir, sessionID string) (*Message, error) {
	if sessionID == "" {
		return nil, nil
	}

	transcriptPath := r.getTranscriptPath(workDir, sessionID)
	return r.readLastMessage(transcriptPath)
}

// GetLastMessages returns the last user and assistant messages from the transcript
// workDir: the working directory of the session
// sessionID: the Claude Code session ID (UUID format)
func (r *Reader) GetLastMessages(workDir, sessionID string) (*LastMessages, error) {
	if sessionID == "" {
		return nil, nil
	}

	transcriptPath := r.getTranscriptPath(workDir, sessionID)
	return r.readLastMessages(transcriptPath)
}

// GetConversation returns the last N user/assistant message pairs from the transcript.
// lastN specifies the number of message pairs to return.
func (r *Reader) GetConversation(workDir, sessionID string, lastN int) ([]Message, error) {
	if sessionID == "" {
		return nil, nil
	}

	transcriptPath := r.getTranscriptPath(workDir, sessionID)
	return r.readConversation(transcriptPath, lastN)
}

// readConversation reads the transcript and returns the last N*2 user/assistant messages.
func (r *Reader) readConversation(filePath string, lastN int) ([]Message, error) {
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	var allMessages []Message
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry transcriptEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}

		content := extractFullContent(&entry)
		if content == "" {
			continue
		}

		allMessages = append(allMessages, Message{
			Type:      entry.Type,
			Content:   content,
			Timestamp: entry.Timestamp,
		})
	}

	// Return last N*2 messages
	maxMessages := lastN * 2
	if len(allMessages) > maxMessages {
		allMessages = allMessages[len(allMessages)-maxMessages:]
	}

	return allMessages, nil
}

// readLastMessages reads the transcript file and returns the last user and assistant messages
func (r *Reader) readLastMessages(filePath string) (*LastMessages, error) {
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	var lastUser *Message
	var lastAssistant *Message
	scanner := bufio.NewScanner(file)
	// Increase buffer size for long lines
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry transcriptEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		// Only process user and assistant messages
		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}

		content := extractContent(&entry)
		if content == "" {
			continue
		}

		msg := &Message{
			Type:      entry.Type,
			Content:   content,
			Timestamp: entry.Timestamp,
		}

		if entry.Type == "user" {
			lastUser = msg
		} else {
			lastAssistant = msg
		}
	}

	if lastUser == nil && lastAssistant == nil {
		return nil, nil
	}

	return &LastMessages{
		User:      lastUser,
		Assistant: lastAssistant,
	}, nil
}

// encodePathForClaude converts a path to Claude Code's directory name format
// Example: /Users/foo/bar → -Users-foo-bar
func encodePathForClaude(path string) string {
	// Replace / with -
	encoded := strings.ReplaceAll(path, "/", "-")
	// The path already starts with /, so after replacement it starts with -
	return encoded
}

// getTranscriptPath returns the full path to the transcript file
func (r *Reader) getTranscriptPath(workDir, sessionID string) string {
	encodedPath := encodePathForClaude(workDir)
	return filepath.Join(r.claudeDir, "projects", encodedPath, sessionID+".jsonl")
}

// transcriptEntry represents a single entry in the JSONL file
type transcriptEntry struct {
	Type      string    `json:"type"`
	Message   msgObject `json:"message"`
	Timestamp string    `json:"timestamp"`
}

// msgObject represents the message field which can have different structures
type msgObject struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // can be string or []contentBlock
	Usage   *Usage `json:"usage,omitempty"`
}

// readLastMessage reads the transcript file and returns the last user/assistant message
func (r *Reader) readLastMessage(filePath string) (*Message, error) {
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	var lastMessage *Message
	scanner := bufio.NewScanner(file)
	// Increase buffer size for long lines
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry transcriptEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		// Only process user and assistant messages
		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}

		content := extractContent(&entry)
		if content == "" {
			continue
		}

		lastMessage = &Message{
			Type:      entry.Type,
			Content:   content,
			Timestamp: entry.Timestamp,
		}
	}

	return lastMessage, nil
}

// extractContent extracts the text content from a transcript entry
func extractContent(entry *transcriptEntry) string {
	if entry.Message.Content == nil {
		return ""
	}

	// User messages: content is a string
	if entry.Type == "user" {
		if str, ok := entry.Message.Content.(string); ok {
			return cleanContent(str)
		}
	}

	// Assistant messages: content is an array of content blocks
	if entry.Type == "assistant" {
		if arr, ok := entry.Message.Content.([]any); ok {
			var texts []string
			for _, item := range arr {
				if block, ok := item.(map[string]any); ok {
					if blockType, ok := block["type"].(string); ok && blockType == "text" {
						if text, ok := block["text"].(string); ok {
							texts = append(texts, text)
						}
					}
				}
			}
			if len(texts) > 0 {
				return cleanContent(strings.Join(texts, " "))
			}
		}
	}

	return ""
}

// extractFullContent extracts the text content without cleaning (preserves newlines).
func extractFullContent(entry *transcriptEntry) string {
	if entry.Message.Content == nil {
		return ""
	}

	if entry.Type == "user" {
		if str, ok := entry.Message.Content.(string); ok {
			return strings.TrimSpace(str)
		}
	}

	if entry.Type == "assistant" {
		if arr, ok := entry.Message.Content.([]any); ok {
			var texts []string
			for _, item := range arr {
				if block, ok := item.(map[string]any); ok {
					if blockType, ok := block["type"].(string); ok && blockType == "text" {
						if text, ok := block["text"].(string); ok {
							texts = append(texts, text)
						}
					}
				}
			}
			if len(texts) > 0 {
				return strings.Join(texts, "\n")
			}
		}
	}

	return ""
}

// cleanContent cleans up the content string for display
func cleanContent(s string) string {
	// Remove newlines and extra whitespace
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\t", " ")

	// Collapse multiple spaces
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}

	return strings.TrimSpace(s)
}

// TruncateMessage truncates a message from the beginning to the specified length
func TruncateMessage(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// TruncateMessageFromEnd truncates a message from the end, keeping the last maxLen characters
// This is useful for assistant messages where the important content (like questions) is often at the end
func TruncateMessageFromEnd(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[len(s)-maxLen:]
	}
	return "..." + s[len(s)-maxLen+3:]
}

// findTranscriptPath locates the JSONL file for a given workDir/sessionID.
// It tries the canonical path first, then falls back to a glob over all
// project directories (sessionID is unique). Returns os.ErrNotExist if not found.
func (r *Reader) findTranscriptPath(workDir, sessionID string) (string, error) {
	if sessionID == "" {
		return "", os.ErrNotExist
	}
	if workDir != "" {
		p := r.getTranscriptPath(workDir, sessionID)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	matches, _ := filepath.Glob(filepath.Join(r.claudeDir, "projects", "*", sessionID+".jsonl"))
	if len(matches) > 0 {
		return matches[0], nil
	}
	return "", os.ErrNotExist
}

// ReadEntries returns transcript entries with Timestamp strictly greater than `since`.
// An entry whose Timestamp equals `since` is excluded — pass the timestamp of the last
// entry already seen to receive only what came after it (no duplicates). String
// comparison is used (Claude Code emits lexicographically sortable RFC3339 timestamps).
// If `since` is empty, returns all entries. workDir may be empty: a glob fallback locates
// the JSONL by sessionID. Returns (nil, nil) if no transcript file exists yet.
func (r *Reader) ReadEntries(workDir, sessionID, since string) ([]Entry, error) {
	if sessionID == "" {
		return nil, nil
	}
	path, err := r.findTranscriptPath(workDir, sessionID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return readEntries(path, since)
}

// LastToolUse returns the last tool_use block. If toolName is non-empty,
// only blocks matching that tool name are considered. Returns (nil, nil)
// if no matching block is found.
func (r *Reader) LastToolUse(workDir, sessionID, toolName string) (*Block, error) {
	entries, err := r.ReadEntries(workDir, sessionID, "")
	if err != nil {
		return nil, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		for j := len(entries[i].Blocks) - 1; j >= 0; j-- {
			b := entries[i].Blocks[j]
			if b.Kind != "tool_use" {
				continue
			}
			if toolName != "" && b.ToolName != toolName {
				continue
			}
			return &b, nil
		}
	}
	return nil, nil
}

// LastToolResult returns the last tool_result block. If toolName is non-empty,
// only results corresponding to a tool_use of that name are considered (matched
// by tool_use_id within the same scan). If onlyErrors is true, only blocks with
// IsError=true are considered. Returns (nil, nil) if not found.
func (r *Reader) LastToolResult(workDir, sessionID, toolName string, onlyErrors bool) (*Block, error) {
	entries, err := r.ReadEntries(workDir, sessionID, "")
	if err != nil {
		return nil, err
	}
	// Build tool_use_id -> tool_name map from a single forward pass for name filtering.
	useNameByID := map[string]string{}
	if toolName != "" {
		for _, e := range entries {
			for _, b := range e.Blocks {
				if b.Kind == "tool_use" && b.ToolUseID != "" {
					useNameByID[b.ToolUseID] = b.ToolName
				}
			}
		}
	}
	for i := len(entries) - 1; i >= 0; i-- {
		for j := len(entries[i].Blocks) - 1; j >= 0; j-- {
			b := entries[i].Blocks[j]
			if b.Kind != "tool_result" {
				continue
			}
			if onlyErrors && !b.IsError {
				continue
			}
			if toolName != "" {
				if useNameByID[b.ToolUseID] != toolName {
					continue
				}
			}
			return &b, nil
		}
	}
	return nil, nil
}

// readEntries reads the JSONL file and returns parsed Entry values, optionally
// filtered by Timestamp > since.
func readEntries(filePath, since string) ([]Entry, error) {
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	var entries []Entry
	scanner := bufio.NewScanner(file)
	// Allow lines up to 16 MiB to accommodate large tool_result payloads.
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw transcriptEntry
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}
		if since != "" && raw.Timestamp != "" && raw.Timestamp <= since {
			continue
		}
		entries = append(entries, Entry{
			Type:      raw.Type,
			Timestamp: raw.Timestamp,
			Blocks:    parseBlocks(&raw),
			Usage:     raw.Message.Usage,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// parseBlocks normalizes a transcriptEntry's content into Blocks. Handles:
//   - user content as plain string -> single text block
//   - user content array containing text and tool_result blocks
//   - assistant content array containing text, thinking, and tool_use blocks
//
// Unknown block kinds are preserved with their declared Kind but otherwise empty.
func parseBlocks(entry *transcriptEntry) []Block {
	if entry.Message.Content == nil {
		return nil
	}
	if str, ok := entry.Message.Content.(string); ok {
		s := strings.TrimSpace(str)
		if s == "" {
			return nil
		}
		return []Block{{Kind: "text", Text: s}}
	}
	arr, ok := entry.Message.Content.([]any)
	if !ok {
		return nil
	}
	var blocks []Block
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := m["type"].(string)
		switch kind {
		case "text":
			text, _ := m["text"].(string)
			blocks = append(blocks, Block{Kind: "text", Text: text})
		case "thinking":
			text, _ := m["thinking"].(string)
			blocks = append(blocks, Block{Kind: "thinking", Text: text})
		case "tool_use":
			b := Block{Kind: "tool_use"}
			b.ToolName, _ = m["name"].(string)
			b.ToolUseID, _ = m["id"].(string)
			if input, ok := m["input"]; ok && input != nil {
				if raw, err := json.Marshal(input); err == nil {
					b.Input = json.RawMessage(raw)
				}
			}
			blocks = append(blocks, b)
		case "tool_result":
			b := Block{Kind: "tool_result"}
			b.ToolUseID, _ = m["tool_use_id"].(string)
			if v, ok := m["is_error"].(bool); ok {
				b.IsError = v
			}
			b.Output = stringifyToolResultContent(m["content"])
			blocks = append(blocks, b)
		default:
			if kind != "" {
				blocks = append(blocks, Block{Kind: kind})
			}
		}
	}
	return blocks
}

// stringifyToolResultContent converts a tool_result's content (string or array of blocks) into a single string.
func stringifyToolResultContent(c any) string {
	switch v := c.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := m["text"].(string); ok {
				parts = append(parts, t)
				continue
			}
			// Fall back to JSON for non-text blocks (e.g., image references).
			if raw, err := json.Marshal(item); err == nil {
				parts = append(parts, string(raw))
			}
		}
		return strings.Join(parts, "\n")
	default:
		if raw, err := json.Marshal(v); err == nil {
			return string(raw)
		}
		return ""
	}
}
