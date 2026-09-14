package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/takaaki-s/jind-ai/internal/procgroup"
)

const (
	MaxIssueCommentBytes = 48 * 1024
	MaxCommentActorBytes = 128
	commentTimeout       = 20 * time.Second
)

var commentKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type IssueComment struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	URL      string `json:"url"`
	Actor    string `json:"actor"`
}

type IssueCommentInspection struct {
	Actor    string        `json:"actor"`
	Existing *IssueComment `json:"existing,omitempty"`
}

// IssueCommentInspector is the read-only half of comment reconciliation.
type IssueCommentInspector interface {
	InspectIssueComment(context.Context, IssueRef, string, string) (IssueCommentInspection, error)
}

// IssueCommentCreator is the mutation capability. It is deliberately separate
// from IssueReader and IssueCommentInspector so ingestion cannot reach it.
type IssueCommentCreator interface {
	CreateIssueComment(context.Context, IssueRef, string, string, string, string) (IssueComment, error)
}

type CommentMutationError struct{ Err error }

func (e *CommentMutationError) Error() string {
	return "GitHub Issue comment outcome is unknown; retry with the same idempotency key to reconcile"
}

func (e *CommentMutationError) Unwrap() error { return e.Err }

type commentCommandRunner interface {
	Run(context.Context, []byte, ...string) ([]byte, []byte, error)
}

type ghCommentCommandRunner struct{}

func (ghCommentCommandRunner) Run(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	cmd := procgroup.CommandContext(ctx, "gh", args...)
	cmd.Env = append(os.Environ(), "GH_HOST=github.com", "GH_PROMPT_DISABLED=1", "GH_PAGER=cat", "PAGER=cat", "NO_COLOR=1")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	stdout := newLimitedBuffer(MaxProviderOutput)
	stderr := newLimitedBuffer(MaxProviderOutput)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if stdout.exceeded || stderr.exceeded {
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("provider output exceeded %d bytes", MaxProviderOutput)
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

type GitHubIssueComments struct {
	runner  commentCommandRunner
	timeout time.Duration
}

func NewGitHubIssueComments() *GitHubIssueComments {
	return &GitHubIssueComments{runner: ghCommentCommandRunner{}, timeout: commentTimeout}
}

func (g *GitHubIssueComments) InspectIssueComment(ctx context.Context, ref IssueRef, key, digest string) (IssueCommentInspection, error) {
	if err := validateCommentInput(ref, key, digest); err != nil {
		return IssueCommentInspection{}, err
	}
	readCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	actorOut, stderr, err := g.runner.Run(readCtx, nil, "api", "user", "--jq", ".login")
	if err != nil {
		return IssueCommentInspection{}, classifyReadError(err, readCtx.Err(), stderr)
	}
	actor := sanitizeInline(string(actorOut))
	if !validGitHubName(actor) || len(actor) > MaxCommentActorBytes {
		return IssueCommentInspection{}, &ReadError{Kind: ReadErrorProvider, Err: fmt.Errorf("provider returned an invalid actor")}
	}

	marker := issueCommentMarker(key, digest)
	jq := fmt.Sprintf(`.[] | select(.body | contains(%s)) | {id,html_url,body,user:.user.login}`, strconv.Quote(marker))
	endpoint := fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", ref.Repository, ref.Number)
	stdout, stderr, err := g.runner.Run(readCtx, nil, "api", "--paginate", endpoint, "--jq", jq)
	if err != nil {
		return IssueCommentInspection{}, classifyReadError(err, readCtx.Err(), stderr)
	}
	existing, err := decodeMatchingComment(stdout, ref, actor, key, digest)
	if err != nil {
		return IssueCommentInspection{}, &ReadError{Kind: ReadErrorProvider, Err: err}
	}
	return IssueCommentInspection{Actor: actor, Existing: existing}, nil
}

func (g *GitHubIssueComments) CreateIssueComment(ctx context.Context, ref IssueRef, actor, key, digest, body string) (IssueComment, error) {
	if err := validateCommentInput(ref, key, digest); err != nil {
		return IssueComment{}, err
	}
	if !validGitHubName(actor) || len(actor) > MaxCommentActorBytes {
		return IssueComment{}, fmt.Errorf("invalid GitHub actor")
	}
	if len(body) == 0 || len(body) > MaxIssueCommentBytes || commentDigest(body) != digest {
		return IssueComment{}, fmt.Errorf("invalid GitHub Issue comment body")
	}
	payload, err := json.Marshal(map[string]string{"body": issueCommentEnvelope(body, key, digest)})
	if err != nil {
		return IssueComment{}, err
	}
	writeCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	endpoint := fmt.Sprintf("repos/%s/issues/%d/comments", ref.Repository, ref.Number)
	stdout, _, runErr := g.runner.Run(writeCtx, payload, "api", "--method", "POST", endpoint, "--input", "-")
	if runErr != nil {
		return IssueComment{}, &CommentMutationError{Err: errors.Join(runErr, writeCtx.Err())}
	}
	comment, err := decodeCreatedComment(stdout, ref, actor, key, digest)
	if err != nil {
		return IssueComment{}, &CommentMutationError{Err: err}
	}
	return comment, nil
}

type ghComment struct {
	ID   json.Number     `json:"id"`
	URL  string          `json:"html_url"`
	Body string          `json:"body"`
	User json.RawMessage `json:"user"`
}

func decodeMatchingComment(data []byte, ref IssueRef, actor, key, digest string) (*IssueComment, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var found *IssueComment
	for {
		var raw ghComment
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode reconciled comment: %w", err)
		}
		if found != nil {
			return nil, fmt.Errorf("provider returned duplicate idempotency markers")
		}
		comment, err := validateGHComment(raw, ref, actor, key, digest)
		if err != nil {
			return nil, err
		}
		found = &comment
	}
	return found, nil
}

func decodeCreatedComment(data []byte, ref IssueRef, actor, key, digest string) (IssueComment, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var raw ghComment
	if err := decoder.Decode(&raw); err != nil {
		return IssueComment{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return IssueComment{}, fmt.Errorf("provider returned trailing comment output")
	}
	return validateGHComment(raw, ref, actor, key, digest)
}

func validateGHComment(raw ghComment, ref IssueRef, actor, key, digest string) (IssueComment, error) {
	id := raw.ID.String()
	parsedID, err := strconv.ParseUint(id, 10, 64)
	if err != nil || parsedID == 0 || strconv.FormatUint(parsedID, 10) != id {
		return IssueComment{}, fmt.Errorf("provider returned an invalid comment id")
	}
	commentActor, err := decodeCommentActor(raw.User)
	if err != nil || commentActor != actor || raw.Body != issueCommentEnvelope(strings.TrimSuffix(raw.Body, "\n\n"+issueCommentMarker(key, digest)), key, digest) {
		return IssueComment{}, fmt.Errorf("provider returned mismatched comment content or actor")
	}
	if commentDigest(strings.TrimSuffix(raw.Body, "\n\n"+issueCommentMarker(key, digest))) != digest {
		return IssueComment{}, fmt.Errorf("provider returned mismatched comment digest")
	}
	if err := validateCommentURL(raw.URL, ref, id); err != nil {
		return IssueComment{}, err
	}
	return IssueComment{Provider: GitHubProvider, ID: id, URL: raw.URL, Actor: actor}, nil
}

func decodeCommentActor(value json.RawMessage) (string, error) {
	var actor string
	if err := json.Unmarshal(value, &actor); err == nil {
		return actor, nil
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(value, &user); err != nil || user.Login == "" {
		return "", fmt.Errorf("provider returned an invalid comment actor")
	}
	return user.Login, nil
}

func validateCommentURL(value string, ref IssueRef, id string) error {
	u, err := url.Parse(value)
	wantPath := fmt.Sprintf("/%s/issues/%d", ref.Repository, ref.Number)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") || u.User != nil ||
		u.RawQuery != "" || u.Path != wantPath || u.Fragment != "issuecomment-"+id {
		return fmt.Errorf("provider returned a mismatched comment URL")
	}
	return nil
}

func validateCommentInput(ref IssueRef, key, digest string) error {
	if err := validateIssueRef(ref); err != nil {
		return err
	}
	if !commentKeyPattern.MatchString(key) {
		return fmt.Errorf("invalid comment idempotency key")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("invalid comment digest")
	}
	return nil
}

func issueCommentMarker(key, digest string) string {
	return fmt.Sprintf("<!-- jind-ai:issue-comment:%s:%s -->", key, digest)
}

func issueCommentEnvelope(body, key, digest string) string {
	return body + "\n\n" + issueCommentMarker(key, digest)
}

func commentDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

var _ IssueCommentInspector = (*GitHubIssueComments)(nil)
var _ IssueCommentCreator = (*GitHubIssueComments)(nil)
