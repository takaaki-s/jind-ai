package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type commentRun struct {
	stdout []byte
	stderr []byte
	err    error
}

type fakeCommentRunner struct {
	runs   []commentRun
	args   [][]string
	stdins [][]byte
}

func (f *fakeCommentRunner) Run(_ context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	f.args = append(f.args, append([]string(nil), args...))
	f.stdins = append(f.stdins, append([]byte(nil), stdin...))
	run := f.runs[len(f.args)-1]
	return run.stdout, run.stderr, run.err
}

func commentFixture(ref IssueRef, actor, key, digest, body string) []byte {
	value := map[string]any{
		"id": 91, "html_url": ref.URL + "#issuecomment-91",
		"body": issueCommentEnvelope(body, key, digest), "user": actor,
	}
	data, _ := json.Marshal(value)
	return data
}

func TestIssueCommentInspectIsReadOnlyAndFindsNoMarker(t *testing.T) {
	ref, _ := ParseGitHubIssueReference("owner/repo#7")
	body, key := "Looks good", "mut_123"
	digest := commentDigest(body)
	runner := &fakeCommentRunner{runs: []commentRun{{stdout: []byte("octocat\n")}, {stdout: nil}}}
	comments := &GitHubIssueComments{runner: runner, timeout: commentTimeout}
	got, err := comments.InspectIssueComment(context.Background(), ref, key, digest)
	if err != nil {
		t.Fatal(err)
	}
	if got.Actor != "octocat" || got.Existing != nil || len(runner.args) != 2 {
		t.Fatalf("inspection=%+v calls=%d", got, len(runner.args))
	}
	for i, args := range runner.args {
		if containsArg(args, "POST") || len(runner.stdins[i]) != 0 {
			t.Fatalf("inspection mutated provider: args=%v stdin=%q", args, runner.stdins[i])
		}
	}
	if strings.Join(runner.args[0], " ") != "api user --jq .login" || !containsArg(runner.args[1], "--paginate") {
		t.Fatalf("args = %v", runner.args)
	}
}

func TestIssueCommentInspectReconcilesExactMarker(t *testing.T) {
	ref, _ := ParseGitHubIssueReference("owner/repo#7")
	body, key := "Done", "mut_123"
	digest := commentDigest(body)
	runner := &fakeCommentRunner{runs: []commentRun{{stdout: []byte("octocat")}, {stdout: commentFixture(ref, "octocat", key, digest, body)}}}
	comments := &GitHubIssueComments{runner: runner, timeout: commentTimeout}
	got, err := comments.InspectIssueComment(context.Background(), ref, key, digest)
	if err != nil {
		t.Fatal(err)
	}
	if got.Existing == nil || got.Existing.ID != "91" || got.Existing.Actor != "octocat" {
		t.Fatalf("inspection = %+v", got)
	}
}

func TestIssueCommentCreateKeepsBodyOutOfArgvAndValidatesResult(t *testing.T) {
	ref, _ := ParseGitHubIssueReference("owner/repo#7")
	body, actor, key := "Ship it", "octocat", "mut_123"
	digest := commentDigest(body)
	created := commentFixture(ref, actor, key, digest, body)
	// The REST create response carries a user object rather than the string
	// projected by Inspect's jq expression.
	var raw map[string]any
	_ = json.Unmarshal(created, &raw)
	raw["user"] = map[string]string{"login": actor}
	created, _ = json.Marshal(raw)
	runner := &fakeCommentRunner{runs: []commentRun{{stdout: created}}}
	comments := &GitHubIssueComments{runner: runner, timeout: commentTimeout}
	got, err := comments.CreateIssueComment(context.Background(), ref, actor, key, digest, body)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "91" || got.URL != ref.URL+"#issuecomment-91" || len(runner.args) != 1 {
		t.Fatalf("comment=%+v args=%v", got, runner.args)
	}
	if strings.Contains(strings.Join(runner.args[0], " "), body) || !containsArg(runner.args[0], "POST") {
		t.Fatalf("body leaked into argv or POST absent: %v", runner.args[0])
	}
	var payload map[string]string
	if err := json.Unmarshal(runner.stdins[0], &payload); err != nil || payload["body"] != issueCommentEnvelope(body, key, digest) {
		t.Fatalf("stdin = %q, err=%v", runner.stdins[0], err)
	}
}

func TestIssueCommentCreateFailureIsUnknownAndDoesNotLeakProviderOutput(t *testing.T) {
	ref, _ := ParseGitHubIssueReference("owner/repo#7")
	body, key := "Ship it", "mut_123"
	digest := commentDigest(body)
	runner := &fakeCommentRunner{runs: []commentRun{{stderr: []byte("secret-token"), err: errors.New("exit 1: secret-token")}}}
	comments := &GitHubIssueComments{runner: runner, timeout: commentTimeout}
	_, err := comments.CreateIssueComment(context.Background(), ref, "octocat", key, digest, body)
	var mutationErr *CommentMutationError
	if !errors.As(err, &mutationErr) || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error = %v", err)
	}
}

func TestIssueCommentRejectsMismatchedAndDuplicateReconciliation(t *testing.T) {
	ref, _ := ParseGitHubIssueReference("owner/repo#7")
	body, key := "Done", "mut_123"
	digest := commentDigest(body)
	bad := commentFixture(ref, "somebody-else", key, digest, body)
	for _, output := range [][]byte{bad, append(append([]byte(nil), commentFixture(ref, "octocat", key, digest, body)...), append([]byte("\n"), commentFixture(ref, "octocat", key, digest, body)...)...)} {
		runner := &fakeCommentRunner{runs: []commentRun{{stdout: []byte("octocat")}, {stdout: output}}}
		comments := &GitHubIssueComments{runner: runner, timeout: commentTimeout}
		if _, err := comments.InspectIssueComment(context.Background(), ref, key, digest); err == nil {
			t.Fatalf("accepted output %s", output)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
