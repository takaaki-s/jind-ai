package git

import (
	"errors"
	"testing"
)

func TestHasSubmodules(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   bool
	}{
		{"", false},
		{"100644 abc 0\tordinary.txt\x00", false},
		{"100644 abc 0\tfile\n160000 fake\x00", false},
		{"160000 abc 0\tsub module\x00", true},
		{"100644 abc 0\tfile\x00160000 def 2\tsub\x00", true},
	} {
		got, err := NewClientWithRunner(&mockRunner{out: []byte(tc.output)}).HasSubmodules("repo")
		if err != nil || got != tc.want {
			t.Fatalf("output=%q got=%t err=%v", tc.output, got, err)
		}
	}
	if _, err := NewClientWithRunner(&mockRunner{err: errors.New("cannot read index")}).HasSubmodules("repo"); err == nil {
		t.Fatal("ignored failed inspection")
	}
}
