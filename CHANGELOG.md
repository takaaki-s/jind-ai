# Changelog

goreleaser assembles per-release notes from Conventional Commits history and
attaches them to the corresponding [GitHub Release](https://github.com/takaaki-s/jind-ai/releases).
This file is the curated overview — highlights per release, not a per-commit
log.

## 0.7.0

### Features

- **Plugin registry** — discover community plugins by name, install them with
  a commit-pinned consent screen. New commands:
  - `jin plugin ls-remote` lists plugins from the registry
    (`https://takaaki-s.github.io/jind-ai-plugin-registry/registry.json`),
    with `--sort`, `--search`, `--refresh`, `--json`, and `--registry` flags.
    The registry document is cached locally for 24 hours with conditional-GET
    revalidation; fetch failures fall back to the stale cache with a warning.
  - `jin plugin install <name>` (e.g. `jin plugin install jind-ai-notifier`)
    resolves through the registry, always clones at the SHA the registry
    recorded at crawl time, and prints a single-screen consent dialog before
    touching anything. `-v/--pin` selects a specific version, `--force`
    overrides an unsatisfied `jin:` compat range, `--yes` skips the prompt.
  - `jin plugin validate` runs the same manifest checks as the registry
    crawler and every plugin repo's CI, so authors get identical feedback
    locally. `--github-actions` emits annotation-formatted output.
- **Unified plugin manifest (`jind-ai-plugin.yaml`)** — the runtime dispatcher
  and the registry crawler now read the same file with the same schema. The
  old `jin-plugin.yaml` / `api_version` shape has been removed.
- **`pkg/plugin/manifest`** — the manifest package is now exported. The
  registry crawler and any third-party tool can validate manifests bit-for-bit
  identically to jin itself.

See [docs/plugin-registry.md](docs/plugin-registry.md) for the full
plugin-registry guide, the manifest schema, install/publish flows, and the
pre-1.0 break policy.

### Breaking changes

`0.7.0` is a pre-1.0 minor bump and carries breaking changes to the plugin
system. See [docs/plugin-registry.md#pre-10-break-policy](docs/plugin-registry.md#pre-10-break-policy)
for the policy in full.

- The plugin manifest file is now `jind-ai-plugin.yaml` (was
  `jin-plugin.yaml`); the `api_version` field is gone and `schema_version: 1`
  takes its place. Existing plugins must migrate the file name, add
  `schema_version` / `name` / `version` / `description` / `jin:`, and move
  `run` / `build` under `install.source.{entrypoint,build[]}`.
- The built-in desktop notifier has been removed from the daemon. Install
  [`jind-ai-notifier`](https://github.com/takaaki-s/jind-ai-notifier) — the
  same notifier repackaged as a plugin — to restore the behaviour.
