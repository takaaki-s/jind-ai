package action

import "testing"

func TestFormatKeyHint(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"single letter passthrough", "n", "n"},
		{"single symbol passthrough", "?", "?"},
		{"single bang passthrough", "!", "!"},
		{"alt backslash", "M-\\", "Alt+\\"},
		{"alt lower letter uppercased", "M-p", "Alt+P"},
		{"alt upper letter preserved", "M-P", "Alt+P"},
		{"ctrl symbol", "C-]", "Ctrl+]"},
		{"ctrl lower letter uppercased", "C-a", "Ctrl+A"},
		{"shift multi-char token passthrough", "S-Tab", "Shift+Tab"},
		{"ctrl alt combined", "C-M-p", "Ctrl+Alt+P"},
		{"alt ctrl combined", "M-C-a", "Alt+Ctrl+A"},
		{"unknown prefix passthrough", "X-p", "X-p"},
		{"dangling single modifier", "M-", "M-"},
		{"dangling chained modifier", "M-M-", "M-M-"},
		{"three modifiers combined", "S-C-M-p", "Shift+Ctrl+Alt+P"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatKeyHint(tc.in); got != tc.want {
				t.Errorf("FormatKeyHint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
