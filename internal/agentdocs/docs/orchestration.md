---
title: Drive a child session end to end
description: The full delegation loop — create a session, send a prompt, wait for it to settle, answer it if it blocks, collect what it actually did, clean up. Read this first.
---

# Drive a child session end to end

You are the orchestrator. A child session is another agent working in its own
tmux pane; you start it, give it work, wait, and check the result. You stay
responsible for what it produces.

## Before anything else

Every command below except `jin docs` needs the daemon:

```bash
jin daemon status >/dev/null 2>&1 || jin daemon start
```

Do this up front rather than reacting to a failure: a daemon that is down
exits 1 like any other error, so there is no distinct code to branch on.

## The loop

```bash
# 1. Create. Always give it a description — it becomes the selector you use
#    for every later command.
jin session new --workdir ~/repos/myapp -d fix-login --json

# 2. Wait for it to come up. `new` returns immediately with status
#    `creating`; `send` refuses anything that is not `idle`.
jin session wait fix-login --status idle

# 3. Send work. `prompt` is an alias for `send`. Always pass
#    `--wait-running`: without it a prompt that reached the input box but
#    was never submitted still exits 0.
jin session send fix-login "run go test ./... and fix what fails" --wait-running

# 4. Wait for it to settle. Include `permission` — a session blocked on a
#    permission prompt is stopped, not working, and waiting only for `idle`
#    will burn the whole timeout on it. If this returns almost instantly,
#    the turn never started — see below.
jin session wait fix-login --until idle,permission --timeout 600 --json

# 4b. If it came back `permission`, answer it. `send` refuses a blocked
#     session; `respond` drives the dialog and returns once it is gone.
#     Neither happens on a codex child — see Statuses below.
jin session result fix-login --json              # what is being asked
jin session respond fix-login --option 1

# 5. Collect. How much of this works depends on the agent kind — see below.
jin session output fix-login --last 1 --json     # the final assistant text
jin session result fix-login --json              # what it actually did

# 6. Clean up when done.
jin session kill fix-login
jin session delete fix-login
```

## Why step 2 is not optional

`jin session new` returns as soon as the session is reserved — status
`creating`, with provisioning and start still running in the background. And
`jin session send` refuses any session that is not `idle`.

Skipping the wait is therefore a race you sometimes lose, with
`session is not idle (current status: creating)`.

Later sends are covered by step 4's wait, but only when it comes back `idle`:
`--until idle,permission` returns on either, and `send` refuses everything but
`idle`. Check the status you got before sending again — on `permission`, answer
with `jin session respond` (step 4b) rather than sending.

Once the session is idle, `send` handles the rest itself: it verifies the
prompt actually reached the input box, retrying while the TUI settles,
before pressing Enter. A successful `send` means the text landed **in the
input box**. It does not mean the child started a turn.

## Why step 3 needs `--wait-running`

Those two can come apart — an agent TUI can take the Enter for something of
its own (a completion popup, a dialog) and leave the prompt sitting unsent.
`send` still exits 0, because the text did land.

The damage is in step 4, not step 3. An unsubmitted prompt leaves the session
on `idle`, so `wait --until idle,permission` returns **immediately** and you
go on to read the previous turn's output as this turn's result. Nothing in
the sequence looks like a failure.

`--wait-running` closes that: it polls until the child leaves idle for
running/thinking/permission and exits 4 if it never does. Exit 4 here means
"nothing confirmed the turn started" — attach and look at the input box
rather than resending blind. See the `exit-codes` doc.

## Inspecting what the child did

`session output` gives you the assistant's text. That is often not enough —
a child that reports "fixed it" may have edited nothing.

`session result` returns structured transcript entries (`text`, `thinking`,
`tool_use`, `tool_result`) so you can check the actions rather than the claim:

```bash
# Only Bash calls
jin session result fix-login --tool Bash --json

# Only failed tool calls
jin session result fix-login --errors-only --json
```

**What comes back depends on the agent kind.** For `claude`, all of it. For
`codex`, the conversation and the tool calls — but the two filters above are
weak there: `--errors-only` cannot see a command that merely exited non-zero,
and every codex tool call is named `exec`. For `opencode`, the conversation,
`thinking` and `usage` all come back and `--tool` is exact (opencode's own
lower-case names, so `--tool bash`), while `--errors-only` is exact only for
`bash`: no other opencode tool records an exit status, so a failure opencode
did not flag itself does not raise the flag.

**An opencode read runs opencode**, which is why it is the one kind where
polling costs something: measured 1.45–1.77s per call whatever the session
size, because the time is opencode's start-up rather than the conversation.
`--since` does not make it cheaper. Read an opencode child rarely and in full
rather than often and incrementally, and expect an error — not an empty result
— if `opencode` is missing from the daemon's PATH.

`session output` reads Claude Code's transcript only, whatever the kind.

Read the `gotchas` doc before acting on a codex or opencode result: the limits
decide what an empty answer is allowed to mean.

### Incremental collection

`--since` is strictly exclusive: pass the last entry's timestamp to get only
what came after it, with no duplicates.

**It can also skip an entry, so do not use it as your only read.** A timestamp
is not a unique key, and when the next entry carries the same one as the entry
you passed, that entry is dropped and never appears in any later poll either.
It is rare — measured at 1 of 112 adjacent entry pairs across 14 codex
sessions, 42 of 51,681 across 242 Claude Code transcripts, and 12 of 478
entries across 34 opencode sessions — but it is silent, and it applies to every
agent kind. When completeness matters more than volume (deciding whether a
child finished, collecting a final answer), read the whole result rather than
the tail.

**`--since` shrinks the answer, not the work, on every kind.** jin reads the
whole conversation and then drops what you already have: the Claude Code reader
walks every line, the codex reader walks every line and only skips decoding the
ones below the bound, and the opencode reader exports the entire session. So a
poll that asks for the last few entries costs what a full read costs, and
polling four times as often costs four times as much for the same conversation.

That has always been true; opencode only makes it visible, because its constant
is a second and a half rather than a millisecond:

| kind | one read |
|---|---|
| `claude` | opens one file |
| `codex` | opens one file |
| `opencode` | ~1.5s — jin runs `opencode export`, and the time is opencode starting up, so a 3-part session costs the same as a 117-part one |

**Poll turns, not seconds.** `send` → `wait --until idle,permission` → `result`
is one read per turn, and a turn takes minutes, so even opencode's second and a
half disappears into it. Do not run `result` on a short timer to watch progress
— that is what `wait` is for, and on an opencode child it starts a process every
time round.

```bash
T1=$(jin session result work --json | jq -r '.entries[-1].timestamp')
# only when the previous wait came back idle, not permission
jin session send work "now also run go vet" --wait-running
jin session wait work --until idle,permission
jin session result work --since "$T1" --json
```

On a `codex` session that has been resumed, read the whole result instead:
whether a resume keeps writing to the same log was never measured, and
`--since` is only as complete as that assumption.

## Statuses

`creating`, `running`, `thinking`, `idle`, `permission`, `stopped`, `deleting`.

Terminal for a turn: `idle` (finished) and `permission` (blocked, waiting to be
answered — `jin session respond`, not `send`). `stopped` means the process is
gone.

**`permission` never appears on a `codex` session.** That adapter maps Codex's
approval hook to `thinking` — the event reports that an approval path opened,
not that a human was asked — and it cannot read Codex's dialog either, so
`respond` refuses it. For a codex child, `--until idle,permission` is
effectively `--until idle`. One really blocked on an approval sits at
`thinking` and stays there: no timer moves a `thinking` session, so your `wait`
spends its whole `--timeout` and the child is still blocked afterwards. Attach
to the pane and answer it.

`stopped` is the one status worth a second look. Nothing re-derives it from
the world, so a stop recorded by mistake stays until a hook disagrees — and an
agent blocked inside one long tool call sends no hook while that call runs.
Meanwhile `wait` reads it as a finished turn and `send` refuses it as a dead
session, so a wrong one costs you the work twice over.

So when a child reads `stopped` without having reported anything, confirm it
against `session result`: the transcript is written by the agent, not by jin,
so a child that is still working keeps producing entries the status knows
nothing about. `tmux_window_name` is not the check — a killed session keeps
its window standing so it can be revived in place, so it is non-empty either
way.

```bash
jin session result <sel> --last 3
sleep 30
jin session result <sel> --last 3   # new entries → still working, status is stale
```

## Accepting the work

Do not forward a child's report as your own conclusion. For code changes,
check `git diff` and run the tests yourself. If the work is short of the bar,
send a correction to the *same* session — it still has the context:

```bash
# only when the previous wait came back idle, not permission
jin session send fix-login "the test still fails on line 42; fix that too" --wait-running
jin session wait fix-login --until idle,permission
```

Answer a session that sits in `permission` with `respond`. What is safe to
answer depends on what is being asked, and only you can tell:

- **A tool approval** — "may I run this command?" — is a capability you already
  have. Answer it.
- **A question about the work** — which approach, whether to keep going — is a
  decision. Answer it when it is yours to make. Escalate when it is not:
  spending the operator's resources, changing what was agreed, anything that
  cannot be undone. `respond` cannot tell these apart, so this is your
  judgment, not a check it performs.

Escalate also when `respond` refuses — a multi-question form, or an agent kind
it does not drive — or when two rounds of correction have not moved the session.

## Running several at once

Sessions are independent. Create them all, then wait on each in turn:

```bash
for d in api web worker; do
  jin session new --workdir ~/repos/$d -d "task-$d" --json
done
for d in api web worker; do
  jin session wait "task-$d" --status idle
  jin session send "task-$d" "..." --wait-running
done
for d in api web worker; do
  jin session wait "task-$d" --until idle,permission --timeout 900
done
```

Creating them all first lets the sessions provision in parallel; the second
loop then only waits out whatever is left.

Give each a distinct `--description`: that is what makes them addressable.
Use `--fleet <name>` to group related sessions.

## Cleaning up

```bash
jin session list --json           # what exists
jin cleanup stopped --dry-run     # preview
jin cleanup stopped               # remove stopped sessions
```

Leaving sessions behind costs a tmux pane each and clutters every later
`session list`.
