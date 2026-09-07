---
title: Traps an orchestrating agent will hit
description: The failures that look like bugs but are not — per-agent limits on result collection, the creating-to-idle race, a send that exits 0 without starting a turn, answering a blocked session, and why a pane capture is not evidence.
---

# Traps an orchestrating agent will hit

## What `session result` answers depends on the agent kind

| kind | `jin session result` |
|---|---|
| `claude` | everything — text, thinking, tool calls, tool results, errors |
| `codex` | the conversation and the tool calls, with the limits below |
| `opencode` | the conversation, tool calls, `thinking` and `usage` — with the limits below |

Check the kind before you decide what an answer means:

```bash
jin session info work --json | jq -r '.agent_kind'
```

**An empty result that exits 0 is ambiguous, and on codex and opencode it can
be wrong for longer than you expect.** It normally means the agent has not
started, so no conversation exists yet. But both of those kinds carry an ID jin
minted before launch and pick one of their own instead; until the agent reports
the real one back, jin has no id worth asking about and answers empty and
successful. On codex, if hooks are not wired for that session, it never stops
doing so. On opencode that window closes as soon as the agent reports its own
id, and from then on a conversation jin cannot fetch is an *error*, not an
empty result. Do not read empty-and-successful as "the child did nothing" on
a codex or opencode session — check `git diff` and the tests, or
`jin pane capture` if you need to see that it is alive at all.

`session output` is a different command and unchanged: it reads Claude Code's
transcript only, and on a `codex` or `opencode` session it reports that no
transcript was found. For a codex or opencode child use `session result`
instead.

What works for every kind: `new`, `send`, `wait`, `kill`, `delete`, `list`,
`info`, and status (`idle` / `thinking`; `permission` on claude and opencode
only — see "Waiting on an approval" below).

When the result is empty, or the limits below say you cannot trust it, go to
the artifacts:

```bash
git -C <workdir> diff              # what actually changed
cd <workdir> && <test command>     # whether it holds up
jin pane capture work              # last resort — see below
```

## `session info`'s last-message fields are a preview, not a result

`jin session info --json` carries `last_user_message` and
`last_assistant_message`. They are what the session list and the TUI row show,
and they now come from the session's own adapter — so a `codex` child fills
them in where it used to leave them blank forever.

Do not collect a child's work from them:

- **Empty means nothing in particular.** These decorate a row that has to
  render either way, so every failure is silent — no reader for that kind, an
  unreadable log, or an agent that genuinely has not spoken all produce the
  same two empty strings, and the command still exits 0. `session result` is
  the call that tells those apart.
- **They are truncated to 500 bytes**, the user preview from the start and the
  assistant preview from the end.
- **They are one message each**, not a turn: tool calls, tool results, and
  thinking are not in them.
- **Injected text is excluded on purpose** — environment blocks, the body of an
  invoked skill, a subagent's own turn. `last_user_message` is what the
  operator typed, which is usually what you want and is not the raw last user
  entry.

```bash
jin session info work --json | jq -r '.last_assistant_message'   # a glance
jin session result work --last 5                                  # the answer
```

## On Codex, `--errors-only` misses the failures you care about

**An empty `--errors-only` on a `codex` session is not evidence that nothing
failed.** Codex's log carries no error flag: a tool call that failed is
recorded exactly like one that succeeded — measured over 14 real sessions,
every one of the 41 calls that carry a status say `completed`, failed patches
included. jin can raise only two signals, both read out of the output text:

- the output's first line starts with `Script failed` (1/41 in that corpus)
- the output's JSON carries `"timed_out": true` (2 occurrences)

**A command that merely exits non-zero is neither.** Codex records
`Script completed` and the exit code appears nowhere in the file, so a broken
build, a failing test run, a rejected lint — the failures you delegate work in
order to find — cannot be detected at all.

So on codex, use `--errors-only` to find *some* failures quickly, never to
conclude there were none. Read the tool results, and run the check yourself:

```bash
jin session result work --json \
  | jq -r '.entries[].blocks[]? | select(.kind=="tool_result") | .output' | tail -50
cd <workdir> && <test command>
```

One codex failure *is* recorded, and it is worth checking for: a turn that died
outright (a usage limit, say) comes back as an entry of type `system` carrying
the reason. 3 of the 14 measured sessions end that way with the agent having
said nothing at all, which otherwise looks exactly like a child still working.

**`--errors-only` does not return it.** That filter keeps entries holding a
failed tool result, and a dead turn is not one — so the single failure Codex
does record is the one thing the failure filter drops. Run this alongside
`--errors-only` every time, not instead of it:

```bash
jin session result work --json | jq -r '.entries[] | select(.type=="system")'
```

## On Codex, `--tool` cannot tell one tool from another

Codex declares one tool name for nearly everything it runs: 41 of the 46 calls
in the measured corpus are named `exec`, and what actually happened — editing a
file, searching the web, running a command — lives inside the call body. jin
reports the name Codex declared, so `--tool exec` matches almost every call and
almost nothing else matches at all. Filter the blocks yourself instead.

## A Codex result carries no usage and no thinking

Token usage is recorded separately from the messages in Codex's log with
nothing tying the two together, so `usage` is always absent on codex entries.
Reasoning summaries are empty (53/53 in the corpus — the real content is
encrypted), so jin emits no `thinking` blocks rather than padding the result
with blank ones. Neither is missing data you can recover by asking again.

## Two Codex paths nobody has measured

- **`--since` after a `codex resume`.** Whether a resumed session appends to
  the same log file or starts a new one was not measured. Incremental
  collection depends on that, so after a resume prefer a full read over
  trusting `--since` to be complete.
- **Waiting on an approval.** No sample of how Codex records a pending
  permission prompt exists, so do not expect a result to show that the child is
  blocked on one. The status will not tell you either — see "Statuses" in the
  `orchestration` doc for what a codex child reports instead, and what to do
  about it.

## An opencode row shows no last message, and that is not a failure

`session info` and the `session list` rows carry the last user and assistant
message for `claude` and `codex` sessions. On `opencode` both stay empty, on
purpose: producing them means running `opencode export`, and those rows are
re-read on a timer, so a list holding opencode sessions would start a process
per row every couple of seconds to fill in a second line.

An empty preview on an opencode row therefore says nothing about the session.
Use `session result` when you need to know what it said.

## An opencode result comes from running opencode, and costs ~1.5s a read

opencode keeps its conversation in a SQLite database, so there is no log to
read: `session result` runs `opencode export` on the session and parses what it
prints. jin records nothing of its own and keeps no copy, so nothing here
depends on the session having been watched while it ran.

What comes back is fuller than codex gives — `thinking` carries the real
reasoning text, entries carry `usage` (opencode splits one turn across several
messages and the entry holds their sum; its reasoning-token count has no field
in jin's shape and is dropped), and `--tool` matches real per-tool names. Those
names are opencode's own and the match is exact, so on an opencode session it
is `--tool bash`, not `--tool Bash`.

Three things follow from the answer being a subprocess, and they change how you
should poll:

- **Every read costs about the same second and a half.** Measured 1.45–1.77s
  per call across sessions from 3 to 117 parts: the time is opencode starting
  up, not the conversation being read. Asking for less does not buy anything —
  `--since` costs exactly as much as a whole read. Prefer fewer, larger polls,
  and keep `session result` off a tight loop against an opencode child.
- **`opencode` has to be on the daemon's PATH**, and it is not enough that the
  session works: jin launches agents through your login shell, which resolves a
  version manager's shims, while the daemon's own PATH may not. When it cannot,
  the read fails with an error saying exactly that — start the daemon from a
  shell where `opencode` resolves.
- **A read that fails is an error, not a short answer.** A truncated or
  unparseable export is refused rather than returned as a conversation that
  merely looks thin. The one quiet case is the pre-mint window above.

Two limits on the content:

- **Revert and undo are not tracked.** opencode can remove a message or a part
  from its own history; jin does not follow those removals, so content the
  child undid can still appear in the result.
- **Four part types never reach an entry:** `file`, `agent`, `patch` and
  `snapshot`. They exist in opencode's schema but appeared nowhere in the
  measured corpus (0 of 672 parts), and mapping a shape nobody has seen is how
  a reader invents content.

A turn that ended in failure comes back as an entry of type `system` carrying
opencode's own error name and message — both cases in the measured corpus were
aborted turns (2 of 124 assistant messages). The check is the same command that
catches codex's dead turns, and it is worth running before you conclude
anything from a thin result:

```bash
jin session result work --json | jq -r '.entries[] | select(.type=="system")'
```

## On opencode, `--errors-only` is exact for bash and partial for everything else

`is_error` comes from two things: an error opencode flagged itself (the one
such call in the 194-call measured corpus is a `read` that could not open a
file), and, for a call opencode called `completed`, the exit status it recorded
alongside it.

The second half is needed because the status alone lies: opencode records a
shell command that exited non-zero as `status: "completed"` — all 5 such calls
in the corpus. jin reads `metadata.exit`, the real exit status, for those.

**Only `bash` carries that field.** 32 of its 33 calls in the corpus record an
exit number and 1 records null; `read`, `grep`, `glob`, `task`, `skill`,
`websearch` and `write` have no exit field at all — 0 of 161 calls, which is
every tool call in the corpus that is not bash.

So `is_error: false` on a `completed` opencode tool result means one of two
different things, and you cannot tell which from the result: the tool reported
an exit status and it was zero, or the tool reports no exit status and jin
cannot tell. Use `--errors-only` to find failed commands; never read an empty
one as "nothing failed", because any non-bash tool that failed without opencode
flagging it is invisible to it.

## An opencode result's timestamps are opencode's, with one correction

Each entry carries the time opencode itself recorded, except where opencode's
clock disagrees with the order of the conversation: there the previous entry's
value is carried forward rather than a new time being invented. Parallel tool
calls are the cause — a call issued first can finish last — and the correction
is rare: 13 of 620 blocks across 34 real sessions need it, the largest going
back 204s.

Two things follow:

- **`--since` can silently drop an opencode entry, exactly as it can on the
  other two kinds.** Carrying a value forward means two entries can share a
  timestamp — 12 of 478 in that corpus — and `--since` is exclusive, so a poll
  landing on such a pair loses the second entry and no later poll returns it.
  opencode is *not* an exception to this; see "Incremental collection" in the
  `orchestration` doc.
- **Parallel tool calls come back in the order they finished**, not the order
  the child issued them. That is what actually happened, and it is the only
  ordering one sequence of timestamps can express.

## Send after `new` needs a wait

`jin session new` returns as soon as the session is reserved, with status
`creating`; provisioning and start continue in the background. `jin session
send` refuses any session that is not `idle`.

So this is a race, and it fails with `session is not idle (current status:
creating)`:

```bash
jin session new --workdir . -d work --json
jin session send work "..." --wait-running   # may fail
```

Wait first:

```bash
jin session new --workdir . -d work --json
jin session wait work --status idle
jin session send work "..." --wait-running
```

A fresh session commonly needs tens of seconds to reach `idle`. Leave the
wait's 300s default alone rather than trimming it to what you measured: a
hook arriving late in startup restarts the clock behind it.

Later sends are covered by the `wait --until idle,permission` you run after
each turn, but only when it comes back `idle` — `send` refuses everything
else. When it comes back `permission`, answer it rather than sending: see below.

## A blocked session is answered with `respond`, not `send`

A session waiting on a prompt has stopped and will not move until it is
answered. Waiting only for `idle` burns the entire timeout on it:

```bash
jin session wait work --until idle,permission   # right
jin session wait work --status idle             # hangs on a blocked session
```

`send` only takes `idle` sessions, so it will usually refuse a blocked one — and
it could not drive a dialog anyway: it proves delivery by finding the text on
screen, and a dialog draws none of what you type. Use `respond`, and read the
choices from the transcript rather than the pane:

```bash
jin session result work --json                  # the question and its options
jin session respond work --option 1             # choose answer 1
jin session respond work --text "use bun"       # where free text is offered
```

`respond` returns only once the prompt is gone from the pane, so exit 0 means
the answer was taken. Exit 4 means it is still there: the keys went out, so
attach and look before answering again.

**Declining leaves the status stale.** Choosing the "No" option answers the
prompt correctly — the tool is rejected, and the transcript records it — but
the agent files no turn-end for a turn it abandoned, so jin goes on reporting
`thinking`. Measured: still `thinking` 90s later, with the pane idle. A
`wait --until idle,permission` after a decline will therefore burn its whole
timeout. Read `session result` to see the rejection, and drive the child with
a fresh `send` rather than waiting for a status that is not coming.

Two limits, both of which refuse without sending anything:

- **Claude Code sessions only.** Other kinds say so; attach and answer those.
- **A form asking several questions at once cannot be answered**, nor can its
  final submit confirmation. Answering one question leaves the form standing,
  so jin cannot tell a half-filled form from an answer that never landed.

## A pane capture is not evidence

`jin pane capture` returns **only what is currently visible** in the pane —
not the scrollback, not what scrolled past while the agent worked. A long
tool output that ran off the top is simply absent, and the capture looks like
a complete picture of a session that did far more than it shows.

Never conclude "the child did nothing" or "the child finished" from a capture.
Use `session result` (within the per-kind limits above), or check the
artifacts directly.

## A child's report is not a result

An agent that says "fixed and tested" may have edited nothing. For code
changes, look at `git diff` and run the tests yourself. `session result
--tool Bash --json` and `--errors-only` exist for exactly this check, and both
mean what they say on `claude`. On opencode `--tool` is exact (with opencode's
own lower-case names) while `--errors-only` is exact only for bash; on codex
both are weak. On either, read the entries and run the tests rather than
trusting a filter.

## Everything except `jin docs` needs the daemon

Start it before anything else, rather than detecting the failure afterwards —
a daemon that is down exits 1, not 3, so there is no code to branch on:

```bash
jin daemon status >/dev/null 2>&1 || jin daemon start
```

`jin docs list` and `jin docs show` work without it — the docs are compiled
into the binary.

## Large prompts are slower, not broken

`send` verifies the text actually reached the agent's input box before
pressing Enter, so a large prompt takes measurably longer to return. That is
the verification working, not a hang — the retry budget scales with prompt
size.

If `send` fails while verifying, the prompt did **not** land — re-send rather
than assuming partial delivery. If it fails after that (a `--wait-running`
timeout, or the session stopping), the text was verified in the input box
and Enter was sent: attach and look before you resend.

One failure tells you neither, and it says so. An error carrying
`its outcome is unknown` is the client giving up on a daemon that may still
be working. Do not re-send on it — attach and look.

## A `send` that exits 0 is not a turn that started

`send` verifies the text reached the input box. It does not check what the
Enter after it did. An agent TUI can take that Enter for itself — a
completion popup is the usual culprit — and leave the prompt unsent while
`send` reports success.

The session then stays `idle`, so the `wait --until idle,permission` behind
it returns at once and you read the previous turn's output as this one's.
Pass `--wait-running` on every send whose result you act on; exit 4 from it
means nothing confirmed the turn started.

When a send did exit 0 but the child looks like it never worked, check for
this before concluding anything about the child:

```bash
jin session info work --json | jq -r '.status'   # still idle right after a send?
jin pane capture work                            # is your prompt still in the box?
```

This is the one question a capture answers well: the input box is on screen
by definition, which is not true of the work you would otherwise be judging
from it (see "A pane capture is not evidence").

Your prompt sitting in the input box — possibly rewritten, e.g. a trailing
`@internal/agent` turned into `@internal/agentdocs/` — is the signature.
`jin` closes the popup before pressing Enter on Claude Code sessions, so you
should not see this there; if you do, attach and press Enter yourself rather
than re-sending the same text, which reproduces it. The prompts that trigger
it end in an `@` path token or consist of a bare `/command` — putting a word
after the `@` token avoids it entirely.

## Whitespace-only prompts are rejected

A prompt consisting only of whitespace (or only box-drawing characters) is
refused, because the verification step has nothing to search the pane for.
Send real text.

## Don't reuse a description across concurrent sessions

Selectors resolve by description substring. Two live sessions described
`test` make every `test` selector ambiguous (exit code 6) until one is
deleted. Name them distinctly up front — see the `selectors` doc.
