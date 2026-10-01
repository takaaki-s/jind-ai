# Task Inbox integration coverage

Run the canonical fixture suite with:

```bash
go test -tags e2e ./internal/daemon -run '^TestE2E_TaskInbox' -count=1 -timeout 120s
```

`make test-e2e` includes this package in CI. The fixture uses real temporary Git
repositories and worktrees, the daemon request dispatcher, session/task stores,
and executable local provider plugins. Agent submission and GitHub Issue/comment
APIs are fakes: no live model, GitHub credentials, or user daemon is needed.
It does not test the Unix-socket transport or a full daemon process restart;
restart assertions reconstruct the managers from their persisted stores.

| Path | Assertions |
| --- | --- |
| Prompt and Issue to cleanup | Isolated execution, completion unseen/seen, keyed check/review retries, PR/merge handoff, worktree cleanup, Task and cleanup journal reload |
| Issue comment | One confirmed comment mutation; body omitted from Task state |
| Failed checks at PR/merge | A new failure reopens unseen attention after acknowledgement; dry-run and confirm are rejected with zero additional provider calls |
| Changes requested at PR/merge | Rejection requires a current reviewed disposition; no additional provider calls |
| Head changed after review | Real new commit invalidates the prior review; no additional provider calls |
| Dirty worktree at PR/merge | Current check/review claims still cannot permit PR dispatch; at merge the prior PR receipt is stale and blocks dispatch before the provider |

The dirty merge path intentionally tests the public sequence, not a forged
dirty-but-current PR receipt. The independent merge cleanliness guard also has
a focused session-manager unit test.

Still outside this canonical suite: provider timeout/unknown-outcome recovery,
same-key retry after interrupted PR/comment mutation, and their duplicate-call
guarantees across reload. Related unit tests are not a substitute for those
end-to-end paths. Real SSH remote execution is a separate test boundary.
