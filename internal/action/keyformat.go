package action

import "strings"

// FormatKeyHint converts a tmux bind-key notation string (e.g. "M-\\",
// "C-M-p") into a human-readable form (e.g. "Alt+\\", "Ctrl+Alt+P").
//
// Simple single-key inputs like "n" or "?" pass through unchanged so the
// same function is safe to call on the whole KeyBindings set.
//
// Unknown modifier prefixes pass through as-is (best-effort, never panics).
func FormatKeyHint(s string) string {
	if s == "" {
		return ""
	}

	var mods []string
	rest := s
	for {
		switch {
		case strings.HasPrefix(rest, "M-"):
			mods = append(mods, "Alt+")
			rest = rest[2:]
		case strings.HasPrefix(rest, "C-"):
			mods = append(mods, "Ctrl+")
			rest = rest[2:]
		case strings.HasPrefix(rest, "S-"):
			mods = append(mods, "Shift+")
			rest = rest[2:]
		default:
			// Dangling modifier ("M-", "M-M-"): input never resolved to a
			// terminal key. Return the raw input so misconfigured bindings
			// surface verbatim rather than a mangled "Alt+" hint.
			if rest == "" && len(mods) > 0 {
				return s
			}
			// Uppercase only when the remainder is a single ASCII letter;
			// symbols and multi-char tokens ("Tab", "\\", "]") pass through.
			if len(mods) > 0 && len(rest) == 1 && rest[0] >= 'a' && rest[0] <= 'z' {
				rest = strings.ToUpper(rest)
			}
			return strings.Join(mods, "") + rest
		}
	}
}
