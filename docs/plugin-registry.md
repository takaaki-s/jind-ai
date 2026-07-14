# Plugin registry

jind-ai's plugin registry lets you discover and install community plugins by
name (`jin plugin install jind-ai-notifier`) rather than by full git URL, with
each install pinned to a specific commit SHA the registry vouched for at
publish time.

The registry itself is a JSON document served from GitHub Pages
(`https://takaaki-s.github.io/jind-ai-plugin-registry/registry.json`), produced
by a crawler that scans GitHub for public repos tagged with the
`jind-ai-plugin` topic. No accounts, no submissions — publish by adding the
topic to your repo.

## Manifest (`jind-ai-plugin.yaml`)

The same file is read by jind-ai at runtime (dispatcher, install) and by the
registry crawler at publish time. One file, one source of truth.

```yaml
schema_version: 1
name: my-notifier
version: 0.1.0
description: Desktop notifications for jin sessions
license: MIT
homepage: https://github.com/foo/my-notifier
jin: ">=0.7.0"
install:
  source:
    build:
      - go build -o bin/notifier ./cmd/notifier
    entrypoint: ./bin/notifier
on: ["status_changed:idle", "status_changed:permission"]
timeout: 30s
```

See the ["Plugins → Manifest" section in the top-level README](../README.md#manifest-jind-ai-pluginyaml)
for the full field reference; every field there is honoured identically by the
registry crawler.

Field notes specific to registry publish:

- **`name`** — matches `^[a-z][a-z0-9-]{1,63}$`; must be unique across the
  registry. Collision policy is described under
  [Publishing](#publishing-a-plugin) below.
- **`description`** — the one-liner shown in `jin plugin ls-remote` search
  results and in the registry entry. Keep it under ~80 characters.
- **`license` / `homepage`** — optional metadata carried into the registry
  entry verbatim.
- **`jin`** — semver constraint on the jin binary (`">=0.7.0"`, `"^0.7"`,
  `">=0.7 <0.9"`). Consumers of the registry evaluate this against their own
  `jin --version`, so declaring a range you have not tested is a footgun.

Unknown fields are ignored by both the runtime and the crawler (a forward-
compatibility guarantee — see the [break-policy](#pre-10-break-policy) below
for what "unknown" is allowed to mean over time).

## Discovering plugins

```bash
jin plugin ls-remote                          # NAME / LATEST / UPDATED / REPO
jin plugin ls-remote --sort updated           # newest updates first
jin plugin ls-remote --search notif           # substring match on name/description
jin plugin ls-remote --refresh                # bypass the 24h local cache
jin plugin ls-remote --json                   # machine-readable
jin plugin ls-remote --registry https://…     # point at an alternate registry
```

The registry document is cached at `$XDG_STATE_HOME/jind-ai/registry/`
(default `~/.local/state/jind-ai/registry/`) for 24 hours. On expiry the next
call re-fetches with `If-None-Match` / `If-Modified-Since` headers, so an
unchanged registry is a cheap `304`. If the fetch fails for any reason
(network down, registry serving an error), a stale cache is used with a
warning printed to stderr so you can still install; use `--refresh` to force a
network round-trip.

## Installing a plugin

```bash
jin plugin install jind-ai-notifier              # registry name → latest_version
jin plugin install jind-ai-notifier -v 0.2.0     # pin a specific version
jin plugin install jind-ai-notifier --refresh    # bypass registry cache
jin plugin install jind-ai-notifier --force      # override an unsatisfied jin compat range
jin plugin install jind-ai-notifier --yes        # skip the consent prompt
```

`jin plugin install` accepts three source shapes:

1. **Registry name** (`jind-ai-notifier`) — resolves through the registry to a
   repo and a pinned commit SHA. This section.
2. **Git URL** (`github.com/owner/repo[@ref]`) — clones directly, bypasses
   the registry. Existing behaviour, no consent-screen changes.
3. **`--link <path>`** — symlinks a local plugin directory (development).
   Trusted outright; no build runs.

A registry-name install always pins to the commit SHA the registry recorded
at crawl time. That SHA is written into `plugins.lock.yaml` on success, so a
subsequent `jin plugin install` / `update` of the same version can never
silently land on a different commit than the one you approved.

### Consent screen

Before doing anything, install shows a single-screen summary:

```
  Plugin:  jind-ai-notifier @ 0.1.0
  Source:  https://github.com/takaaki-s/jind-ai-notifier@a1b2c3d4e5f6
  Kind:    (unverified community plugin)
  Compat:  jin >=0.7.0  (you have 0.7.0)  ✓

  Installation will:
    1. clone https://github.com/takaaki-s/jind-ai-notifier at a1b2c3d4e5f6 into
       /home/you/.local/share/jind-ai/plugins/jind-ai-notifier/
    2. run: go build -o bin/notifier ./cmd/notifier

Install? [y/N]:
```

The screen is deliberately flat — everything a user needs to decide sits on
one page, no drill-in required. The exact shell commands that will run are
shown verbatim (no cosmetic simplification); the community-plugin marker is
always present because the registry does no verification; and the jin compat
verdict (✓ / ✗) is computed against the running binary.

Skip the prompt with `--yes`. When compat fails, install aborts unless
`--force` is passed — the ✗ verdict is shown either way before that decision.

If clone or build fails, install rolls back atomically: nothing is left in
the plugins directory, and the lock file is untouched. Build output is kept
at `~/.local/state/jind-ai/plugin-logs/<name>-build.log`.

## Publishing a plugin

The registry is topic-based: any public GitHub repo with the `jind-ai-plugin`
topic and a valid `jind-ai-plugin.yaml` at its root is picked up by the next
crawler run (default cadence: every 6 hours). No form to fill in, no PR to
open.

Checklist:

1. Publish the code to a public GitHub repo.
2. Add the `jind-ai-plugin` topic (Repo → About → gear icon → Topics).
3. Place `jind-ai-plugin.yaml` at the repo root.
4. Run `jin plugin validate .` locally — this is exactly what the crawler
   runs, so a green local check is a strong predictor of registry inclusion.
5. Cut a semver tag (`v0.1.0` etc.) — the crawler prefers the latest GitHub
   Release, and falls back to the default branch HEAD if none exists.

For a working starter, use the [`jind-ai-plugin-template-notifier`](https://github.com/takaaki-s/jind-ai-plugin-template-notifier)
template repository ("Use this template" — do not fork). It ships with a
manifest that already passes `jin plugin validate` and a CI snippet that runs
the same validation on every PR.

### Name conflict policy

Two repos can declare the same `name`. The crawler resolves this
deterministically so a plugin's identity is stable across runs:

- The **previous crawl's owner** of the name keeps it as long as its repo
  still exists and still declares the topic. A new repo claiming the same
  name is rejected.
- Among new claimants in the same crawl run, the repo with the earliest
  `created_at` wins (GitHub's `repos.created_at`).
- If the current owner disappears (repo deleted, topic removed, manifest
  becomes invalid), the name enters a **30-day grace period** during which no
  other repo can take it. The name reappears as an orphaned entry with
  `orphaned_since` set.
- After 30 days of orphaning, the name is released; the next crawl assigns it
  to the earliest-created new claimant.

This is enforced by the crawler alone — jind-ai clients trust whatever the
registry says.

## `jin plugin validate`

`jin plugin validate` is the entry point that runs the exact checks the
crawler runs. Use it locally before publishing, and in your plugin repo's CI:

```bash
jin plugin validate                       # defaults to .
jin plugin validate ./some/plugin/dir
jin plugin validate --github-actions      # emit ::error / ::warning annotations
jin plugin validate --fail-on-warning     # treat WARN as non-zero exit
```

Exit codes: `0` = pass (or WARN only), `1` = ERROR (or WARN with
`--fail-on-warning`). Under `--github-actions`, ERROR lines print as
`::error file=jind-ai-plugin.yaml,line=N::…` and a markdown summary is
appended to `$GITHUB_STEP_SUMMARY` when set.

The template repo's `.github/workflows/ci.yaml` downloads a pinned jin
release and invokes this command directly — there is no separate GitHub
Action to install or configure. See the template repo's README for the
snippet.

## Pre-1.0 break policy

jin is currently pre-1.0. It follows the pre-1.0 semver convention:

- **Minor bumps (`0.X.0`) may include breaking changes** — including
  changes to the plugin API, manifest field semantics, environment variables
  passed to plugins, and the shape of the registry JSON document.
- **Patch bumps (`0.X.Y`) are bug fixes only.**
- **`1.0` is reserved for the moment the plugin API is committed to
  stability.** After 1.0, breaking changes will require a major bump; before
  1.0, they will not.

Concretely for plugin authors, this means:

- Declare `jin: ">=0.7.0"` (or a tighter range like `"^0.7"`) so jind-ai can
  refuse to load your plugin on incompatible versions. Install-time checks
  are fail-closed; dispatch-time checks are fail-open (logged once, marked
  `incompatible` in `jin plugin list`).
- Expect to re-test your plugin on every jin minor bump during pre-1.0. When
  a bump lands, the CHANGELOG's Features section will call out any changes
  that affect plugins.
- Treat any unknown environment variable, JSON field, or CLI flag as
  something to ignore rather than error on — jind-ai only adds within a
  `schema_version`, and this contract is what makes minor bumps survivable.

The `schema_version` in `jind-ai-plugin.yaml` and in the registry JSON is
orthogonal to jin's own version. It bumps only when a field is removed or
its semantics change, and the validator supports the current version plus
two generations back.
