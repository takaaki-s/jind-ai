package cmd

import (
	"strings"
	"testing"
)

func TestReviewCleanupCommandRegisteredAndRequiresExplicitMode(t *testing.T) {
	if got, _, err := sessionCmd.Find([]string{"cleanup"}); err != nil || got != reviewCleanupCmd {
		t.Fatal("session cleanup command is not registered")
	}
	_ = reviewCleanupCmd.Flags().Set("dry-run", "false")
	_ = reviewCleanupCmd.Flags().Set("confirm", "false")
	_ = reviewCleanupCmd.Flags().Set("idempotency-key", "")
	if err := reviewCleanupCmd.RunE(reviewCleanupCmd, []string{"task"}); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("mode error = %v", err)
	}
}

func TestReviewCleanupCommandRequiresKeyForConfirm(t *testing.T) {
	_ = reviewCleanupCmd.Flags().Set("dry-run", "false")
	_ = reviewCleanupCmd.Flags().Set("confirm", "true")
	_ = reviewCleanupCmd.Flags().Set("idempotency-key", "")
	t.Cleanup(func() {
		_ = reviewCleanupCmd.Flags().Set("confirm", "false")
		_ = reviewCleanupCmd.Flags().Set("idempotency-key", "")
	})
	if err := reviewCleanupCmd.RunE(reviewCleanupCmd, []string{"task"}); err == nil || !strings.Contains(err.Error(), "idempotency-key") {
		t.Fatalf("key error = %v", err)
	}
}
