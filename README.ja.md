[English](README.md) | **日本語**

# jind-ai

複数の対話エージェントセッションを同時に稼働させ、一元管理するための CLI ツール
(Claude Code を first-class citizen としてサポート。他エージェントは
`internal/agent/<kind>/` にアダプタを追加することで拡張可能)。

<img height="200" alt="Image" src="https://github.com/user-attachments/assets/9c32b796-991d-470b-8d23-58e10e99c1c4" />

https://github.com/user-attachments/assets/62e9d64a-aa7d-42f8-8edf-03f724fe0ee4

## 対応エージェント

| Kind | CLI | 備考 |
|---|---|---|
| `claude` (デフォルト) | [Claude Code](https://claude.com/product/claude-code) 2.x | first-class サポート。`--session-id` / `--resume` と CC のネイティブ hook で状態追跡。 |
| `codex` | [OpenAI Codex CLI](https://github.com/openai/codex) 0.144+ | spawn ごとに `-c hooks.X=[...]` で hook を注入。初回のみ `/hooks` ダイアログでトラスト承認が必要 (詳細: [docs/gotchas.md](docs/gotchas.md#codex-adapter))。Codex には `--session-id` 相当がないため、session UUID は `SessionStart` hook で受け取って daemon 側に書き戻す。 |

セッションごとに adapter を選ぶ:

```bash
jin session new --agent codex --workdir ~/repos/myrepo
```

`~/.config/jind-ai/config.yaml` に `default_agent: codex` を書けば常時デフォルトを切り替えられる。TUI の作成フォームは現状このデフォルトを使う (picker 追加は後続 PR を予定)。

## 特長

- **複数セッション管理**: 複数の Claude Code セッションをバックグラウンドで同時実行
- **tmux ネイティブ**: セッションの実体は tmux 上で動く独立ペイン。普段お使いの `~/.tmux.conf` やカスタムキーバインド、ステータスバー、コピーモード等がそのまま使える
- **UI / ロジック分離アーキテクチャ**: セッション管理・状態遷移・hook 処理などのロジックは全て daemon に集約。TUI は Unix socket 経由で daemon を叩く薄いクライアントで、セッション管理ロジックを持たない。同じ IPC を叩けば TUI を別実装（Web UI・エディタ拡張等）に差し替えることも理論上可能（詳細は [docs/architecture.md](docs/architecture.md) / [docs/ipc-protocol.md](docs/ipc-protocol.md)）
- **TUI**: セッション一覧・状態確認・操作を対話的に行えるターミナル UI
- **アタッチ/デタッチ**: セッション間を素早く切り替え（`Ctrl+]` でデタッチ）
- **リアルタイム状態追跡**: 作業ディレクトリ・ブランチ・最新メッセージをリアルタイム表示
- **セッションフィルタ・ページング**: `M-f`（Alt+f、旧: `/`）でファジー検索ポップアップを起動、セッション名・ディレクトリ・ブランチ・フリート・エージェント種別を横断検索
- **プラグイン**: セッションの状態変化やオンデマンドで独自のシェルスクリプトを実行可能。例: `jin plugin install github.com/takaaki-s/jind-ai-notifier` でデスクトップ通知を有効化

## インストール

### GitHub Releases からダウンロード

[Releases ページ](https://github.com/takaaki-s/jind-ai/releases)からお使いの OS/アーキテクチャに合ったバイナリをダウンロードしてください。

```bash
# 例: Linux amd64
curl -Lo jind-ai.tar.gz https://github.com/takaaki-s/jind-ai/releases/latest/download/jind-ai_0.1.0_linux_amd64.tar.gz
tar xzf jind-ai.tar.gz
sudo mv jin /usr/local/bin/
```

### Go install

```bash
go install github.com/takaaki-s/jind-ai/cmd/jin@latest
```

### ソースからビルド

```bash
git clone https://github.com/takaaki-s/jind-ai.git
cd jind-ai
make build    # bin/jin にビルド
make install  # $GOPATH/bin にインストール
```

## クイックスタート

### 1. デーモンを起動

```bash
jin daemon start
```

### 2. TUI を起動

```bash
jin ui
```

### 3. セッションを作成・アタッチ

TUI 内で `n` キーを押してセッション作成、`Enter` でアタッチ。

`Ctrl+]` でデタッチして TUI に戻ります。

## セッションステータス

セッションの状態は Claude Code の [hooks](https://docs.anthropic.com/en/docs/claude-code/hooks) によりイベントドリブンで検知されます。

| ステータス | アイコン | 検知方法 | 説明 |
|-----------|---------|---------|------|
| `thinking` | ⚡ | `UserPromptSubmit` hook | 処理中 |
| `permission` | ? | `Notification` hook | 許可待ち |
| `running` | ▶ | 内部設定 | 実行中 |
| `creating` | + | 内部設定 | 作成中（CC起動中） |
| `idle` | ○ | `Stop` hook | 入力待ち |
| `stopped` | ■ | プロセス死亡検知 | 停止済み |

## CLI コマンド

### デーモン管理

```bash
jin daemon start   # デーモン起動
jin daemon stop    # デーモン停止
jin daemon status  # 状態確認
```

### セッション管理

```bash
# セッション作成（TUI で対話的に作成 - 推奨）
jin session new

# セッション作成（作業ディレクトリ指定）
jin session new --workdir ~/repos/myrepo

# セッション一覧
jin session list

# JSON形式で出力（スクリプト / LLM連携用）
jin session list --json

# セッションにアタッチ
jin session attach <session-name>

# セッションの詳細情報を取得
jin session info <session-name>

# セッションにプロンプトを送信
jin session send <session-name> "プロンプト"

# セッションが idle になるまで待機（デフォルトタイムアウト: 300秒）
jin session wait <session-name>
jin session wait <session-name> --timeout 60

# 最後のアシスタントメッセージを取得
jin session output <session-name>

# 直近 N 往復の会話を取得
jin session output <session-name> --last 3

# セッション終了
jin session kill <session-name>

# セッション削除
jin session delete <session-name>

# 停止済みセッションの一括削除
jin cleanup stopped
jin cleanup stopped --dry-run   # 削除対象の確認
```

> **エイリアス**: `session` は `sess` でも可（例: `jin sess list`）。`list` は `ls`、`delete` は `rm` でも可。

### LLM API（スクリプト / 自動化）

以下のコマンドは `--json` フラグに対応しており、スクリプトや他の LLM エージェントとの連携が可能です。

```bash
# 全セッションコマンドが --json に対応
jin session list --json
jin session new --workdir ~/repos/myrepo --json
jin session info <session-name> --json
jin session kill <session-name> --json

# プロンプト送信 → 完了待機 → 出力取得
jin session send <session-name> "テストを修正して" --json
jin session wait <session-name> --timeout 120 --json
jin session output <session-name> --json

# パイプライン例: プロンプト送信 → 待機 → 出力取得
jin session send my-session "main.go をリファクタリング"
jin session wait my-session --timeout 300
jin session output my-session --last 1
```

#### 終了コード

| コード | 意味 |
|--------|------|
| 0 | 成功 |
| 1 | 一般エラー |
| 2 | セッションが見つからない |
| 3 | デーモン未起動 |
| 4 | タイムアウト（`session wait`） |

### ユーティリティ

```bash
jin session workdir <session-name>    # セッションの作業ディレクトリパスを出力
jin session edit <session-name>       # EDITOR でセッションの作業ディレクトリを開く
```

以下のシェル関数を定義すると便利です：

```bash
# セッションの作業ディレクトリに移動
cc-cd() { cd "$(jin session workdir "$1")"; }

# fzf でセッションを選択して作業ディレクトリに移動
cc-cdf() {
  local session
  session=$(jin session list | tail -n +2 | fzf --height 40% --reverse | awk '{print $1}')
  [[ -n "$session" ]] && cd "$(jin session workdir "$session")"
}

# fzf でセッションを選択してアタッチ
cc-attach() {
  local session
  session=$(jin session list | tail -n +2 | fzf --height 40% --reverse | awk '{print $1}')
  [[ -n "$session" ]] && jin session attach "$session"
}
```

### シェル補完

```bash
# bash
source <(jin completion bash)

# zsh
source <(jin completion zsh)

# fish
jin completion fish | source
```

## 設定

[XDG Base Directory Specification](https://specifications.freedesktop.org/basedir-spec/) に準拠して、ファイルが config / state / runtime に分かれて保存されます:

```
$XDG_CONFIG_HOME/jind-ai/      （デフォルト: ~/.config/jind-ai）
└── config.yaml                # 設定ファイル

$XDG_STATE_HOME/jind-ai/       （デフォルト: ~/.local/state/jind-ai）
├── state.yaml                 # 状態ファイル（前回使用したリポジトリ等）
├── sessions/                  # セッションデータ
├── hooks-settings.json        # Claude Code フック設定（自動生成）
├── plugins.lock.yaml          # インストール済みプラグイン台帳（下記のプラグイン節を参照）
├── plugin-logs/               # プラグインごとの dispatch/run とビルド出力
├── daemon-debug.log           # デーモンデバッグログ（JIN_DEBUG=1 時）
├── hook-debug.log             # フックデバッグログ（JIN_DEBUG=1 時）
└── plugin-debug.log           # プラグインディスパッチャーのデバッグログ（JIN_DEBUG=1 時）

$XDG_DATA_HOME/jind-ai/        （デフォルト: ~/.local/share/jind-ai）
└── plugins/                   # インストール済みプラグイン（下記のプラグイン節を参照）

$XDG_RUNTIME_DIR/jind-ai/      （未設定時のフォールバック: $TMPDIR/jind-ai-<uid>）
└── daemon.sock                # デーモンソケット
```

### 設定例 (`~/.config/jind-ai/config.yaml`)

```yaml
# キーバインドのカスタマイズ（省略時はデフォルト値を使用）
keybindings:
  # セッション一覧画面
  up: ["up", "k"]
  down: ["down", "j"]
  attach: ["enter"]
  new: ["n"]
  kill: ["x"]
  delete: ["d"]
  refresh: ["r"]
  search: ["M-f"]         # セッションフィルタポップアップ（ファジー検索）を開く
                          # デフォルトは M-f (Alt+f)。修飾キー必須 — 素の文字を
                          # 割り当てると表示ペインに届かず、エージェント側の
                          # スラッシュコマンド等を奪ってしまう。
                          # 旧デフォルトの `/` に戻したい場合は ["/"]
                          # （ただしペイン内 `/` 入力が奪われる）
  vscode: ["v"]
  quit: ["q", "ctrl+c"]
  help: ["?"]
  # セッション作成フォーム
  next_field: ["tab"]
  prev_field: ["shift+tab"]
  submit: ["enter"]
  cancel_form: ["esc"]
  # アタッチ中
  detach: ["ctrl+]"]  # デフォルト: ctrl+]
                       # サポートキー: ctrl+^, ctrl+], ctrl+\, ctrl+g

# ポップアップサイズ (percent、1-100 の int)。省略した項目はデフォルト値
# (create/session_filter/action = 70-80) が使われます。全項目と経路は
# docs/tui-guide.md#popup-sizes を参照。
popups:
  create:         { width: 80, height: 80 }
  session_filter: { width: 70, height: 70 }
  help:           { width: 60, height: 60 }
  action:         { width: 70, height: 70 }
  plugin_default: { width: 70, height: 70 }
  plugins:                                # プラグイン単位の上書き
    # my-notifier:  { width: 40, height: 20 }
```

### Worktree の作成先

`jin session new --worktree` はデフォルトで `$XDG_STATE_HOME/jind-ai/worktrees/{name}`（通常 `~/.local/state/jind-ai/worktrees/` 配下）に worktree を作成します。`config.yaml` の `worktree.base_dir` で任意の場所に変更できます:

```yaml
worktree:
  # リポジトリ単位でまとめて配置
  base_dir: "${HOME}/ghq/worktrees/{repo}/{name}"
```

その他の配置例:

```yaml
# 開発ディレクトリ配下にフラットに置く
worktree:
  base_dir: "${HOME}/dev/worktrees/{name}"

# 固定ルート配下（{repo} を使わない）
worktree:
  base_dir: "/mnt/fast/worktrees/{name}"
```

テンプレート変数:

| 変数 | 展開結果 |
|------|----------|
| `{name}` | worktree 名（例: `jin-abcd1234` / `--name` で指定した名前） |
| `{repo}` | 元リポジトリのベース名 |
| `${VAR}` | 環境変数（`os.ExpandEnv` に準拠） |

展開結果は絶対パスである必要があります。未知の `{xxx}` はセッション作成時にエラーになります。

### Worktree のブランチ命名

worktree 作成時には対応するブランチも自動生成されます。命名を制御する 2 つの設定:

```yaml
worktree:
  branch_prefix: "topic/"   # デフォルト: "jin/"。"" にするとプレフィックス無し。
  default_branch: "main"    # 起点ブランチのフォールバック。デフォルト: ""（フォールバック無し）
```

- **`branch_prefix`** — 自動生成された worktree 名の前に付与されてブランチ名になります。worktree 名先頭の `jin-` は事前に除去されるため、デフォルト設定では `jin-abcd1234` は `jin/jin-abcd1234` ではなく `jin/abcd1234` になります。`jin session new --worktree-branch <name>` でブランチを明示指定した場合は無視されます。
- **`default_branch`** — リポジトリの起点ブランチを自動検出**できなかった場合のみ**使用されます。検出は `refs/remotes/origin/HEAD` を参照するため、origin/HEAD が未設定のクローン（一部の tarball、`git clone --no-checkout`、古いクローン等）ではフォールバックが発動します。検出も失敗し `default_branch` も空だと、`cannot detect default branch` エラーでセッション作成が失敗します。

worktree 作成自体は**完全オフライン**で動作します — ローカルの `origin/<base>` からブランチを切るだけでネットワークアクセスは行いません。重いリポジトリでもセッション作成のたびに fetch のコストを払わずに済みます。最新のリモート tip から worktree を切り出したい場合は、`jin session new --worktree` の前に元リポジトリで `git fetch origin <base>` を実行するか、下記の [Worktree Post-Create Hook](#worktree-post-create-hook) 内で fetch を仕込んでください。

## TUI キーバインド

### セッション一覧画面

| キー | 動作 |
|------|------|
| `↑/k` | 上に移動 |
| `↓/j` | 下に移動 |
| `←/h` | 前のページ |
| `→/l` | 次のページ |
| `M-f` | セッションフィルタを開く（ファジー検索ポップアップ）— 詳細は [アウター tmux — セッションフィルタ](#アウター-tmux--セッションフィルタ) |
| `Enter` | セッションにアタッチ |
| `n` | 新規セッション作成 |
| `x` | セッション終了 |
| `d` | セッション削除 |
| `r` | 一覧更新 |
| `v` | VS Codeで開く |
| `?` | ヘルプ表示 |
| `q` | 終了 |

### セッション作成フォーム

| キー | 動作 |
|------|------|
| `Tab` | 次のフィールドへ移動 |
| `Shift+Tab` | 前のフィールドへ移動 |
| `Enter` | セッション作成 |
| `Esc` | キャンセル |

アタッチ中は `Ctrl+]`（デフォルト）でデタッチして TUI に戻ります。
`config.yaml` の `keybindings.detach` で変更可能です。

サポートされるデタッチキー:

| キー | 説明 |
|------|------|
| `ctrl+]` | デフォルト |
| `ctrl+^` | Ctrl+Shift+6 |
| `ctrl+\` | Ctrl+バックスラッシュ |
| `ctrl+g` | Ctrl+G |

### アウター tmux — セッションフィルタ

`M-f`（Alt+f、デフォルト）でセッションフィルタを開きます。ファジー検索ポップ
アップで、数文字入力して `Enter` を押せば即座にそのセッションへアタッチでき
ます。outer tmux（`jin-mgr`）のルートキーテーブルにバインドされるため、
セッション一覧（左ペイン）・アタッチ中のエージェント（右ペイン）どちらからでも
起動できます。検索対象はセッションの説明・作業ディレクトリ・現在の作業ディレ
クトリ・git ブランチ・フリート・エージェント種別の 6 フィールド（
[sahilm/fuzzy](https://github.com/sahilm/fuzzy) による subsequence マッチ、
smart-case、スコア順ランキング）。`Esc` でポップアップを閉じても TUI の状態
は変わりません。`↑`/`↓` または `Ctrl+P`/`Ctrl+N` でカーソル移動します。

デフォルトが `/` から `M-f` に変更されたのは、outer tmux のルートに素の文字を
バインドすると表示ペインでの `/` 入力（Claude Code のスラッシュコマンド、less
や vim の検索など）を奪ってしまうためです。アクションパレット（`M-p`）にも
「session filter」エントリがあるため、ショートカット未設定でも起動できます。

```yaml
keybindings:
  search: ["ctrl+p"]      # Ctrl+p に変更
  # search: ["/"]         # 旧デフォルトの `/` に戻す（表示ペインの `/` が奪われる）
  # search: []            # 完全に無効化（bind-key を発行しない）
```

## Claude Code Hooks

jind-ai はセッションの状態検知に Claude Code の hooks を使用します。**Hooks は自動で設定されます** — 手動設定は不要です。

セッション起動時に jind-ai が `$XDG_STATE_HOME/jind-ai/hooks-settings.json`（デフォルト `~/.local/state/jind-ai/hooks-settings.json`）を生成し、`claude --settings` 経由で Claude Code に渡します。

各 hook の役割:

| Hook Event | 役割 |
|-----------|------|
| `UserPromptSubmit` | ユーザーがプロンプトを送信 → セッションを `thinking` に |
| `PostToolUse` | ツール実行完了 → セッションを `thinking` に（`permission` 状態からの復帰） |
| `Stop` | Claude のターン終了 → セッションを `idle` に（`JIN_NOTIFY_KIND=task-complete` をプラグインに配信） |
| `Notification` | 権限要求等 → セッションを `permission` に（`JIN_NOTIFY_KIND=permission` をプラグインに配信） |

## Worktree Post-Create Hook

`jin session new --worktree` でセッションを作成した際、worktree 生成直後にセットアップ用スクリプトを自動実行できます。依存関係のインストール、`.env` のコピー、submodule の初期化など、worktree を作るたびに手作業でやっていた手順を丸ごと自動化できます。

### スクリプトの配置

**元リポジトリ**（worktree 側ではなく）の `.jin/worktree-post-create.sh` に置きます。常に `bash` 経由で起動されるため `chmod +x` は不要。ファイルが存在しなければ hook は無音でスキップされます。

```bash
#!/usr/bin/env bash
set -euo pipefail

# 親リポジトリの .env をコピー（git 管理外）
cp "$JIN_REPO_ROOT/.env" "$JIN_WORKTREE_PATH/.env" 2>/dev/null || true

# 依存関係インストール
pnpm install
```

### 環境変数

| 変数 | 内容 |
|------|------|
| `JIN_WORKTREE_PATH` | 作成された worktree の絶対パス |
| `JIN_WORKTREE_BRANCH` | worktree でチェックアウトされているブランチ |
| `JIN_WORKTREE_BASE` | worktree の起点となったベースブランチ |
| `JIN_SESSION_ID` | 作成中セッションの UUID |
| `JIN_SESSION_NAME` | `--name` で指定されたセッション名（省略時は空。自動導出名は hook 実行後に確定するため） |
| `JIN_REPO_ROOT` | 元リポジトリの絶対パス |

### セキュリティ: allowlist

スクリプトはリポジトリにチェックインされる shell script なので、jind-ai は明示的に信頼されたリポジトリでない限り実行しません（direnv 流の allow モデル）。信頼はスクリプトの SHA256 で紐付けされ、スクリプトを編集すると再度信頼が必要になります。

```bash
jin worktree allow    # カレントリポジトリを信頼（スクリプト全文表示 + 確認プロンプト）
jin worktree revoke   # 信頼を取り消し
jin worktree status   # カレントリポジトリの信頼状態を表示
jin worktree list     # 信頼済みリポジトリを一覧
```

スクリプトが存在するが未信頼（または変更検知された）場合、hook は警告付きでスキップされ、worktree 自体は作成されて Claude は通常通り起動します。TUI からセッション作成した場合は popup 上で「許可する / スキップして作成 / やめる」の 3 択が表示されます。

### hook のスキップ

- `jin session new --worktree --no-hook` — このセッションだけ hook をスキップ
- `~/.config/jind-ai/config.yaml` に `worktree.hook_enabled: false` — 全リポジトリで hook を無効化
- `worktree.hook_timeout: <秒>` — タイムアウト変更（デフォルト `300`）。超過時はプロセスグループに `SIGTERM` を送り、5 秒の grace 後に生存していれば `SIGKILL`。

### 失敗時の挙動

hook が非ゼロ終了またはタイムアウトすると、worktree とブランチは rollback され、`jin session new` は非ゼロ exit code で失敗します。stdout/stderr は `~/.local/state/jind-ai/hook-logs/<session-id>.log` に保存され、rollback 後も診断のために残ります。

## プラグイン

jind-ai では、セッションのステータス変化に反応して、あるいはオンデマンドで、任意のシェル実行可能なプラグインを実行できます。プラグインはマニフェストとエントリーポイントのスクリプトを持つディレクトリです。jind-ai はスクリプトが何をするかには関与せず、いつ実行されどんな環境を受け取るかだけを管理します。

### 2 通りの実行方式

- **Event listener（イベントリスナー）** — マニフェストの `on:` マッチャー経由で `status_changed` を購読します。通知、ロギング、CI トリガーなど、非対話的な用途に向いています。注意: イベントはステータスが実際に変化した時のみ発火します。ステータス遷移を伴わない通知（既に idle の状態での再停止など）は dispatch されません。
- **Action（アクション）** — `jin plugin run <name> [--session <selector>]` で明示的に起動します。ポップアップベースの diff レビュー UI のような、対話的なワークフローに向いています。`on: []` を指定すると action 専用のプラグインになります。`--session` を省略すると **グローバル action** になり、セッション由来の環境変数はすべて空になります。action 実行時は (global・session 指定を問わず) 呼び出し元の CLI が tmux クライアント内にいた場合、`JIN_CALLER_TMUX_SOCKET` / `JIN_CALLER_TMUX_PANE` が起動元を示します。

どちらのエントリーポイントも同じ `run:` コマンドを同じ環境で実行します。違いはトリガーだけです。

### マニフェスト（`jind-ai-plugin.yaml`）

このファイルをプラグインディレクトリのルートに配置します。ランタイム（ディスパッチャー）と publish 時（レジストリクローラー）が同じマニフェストを読みます。単一ファイル、単一の真実です。

```yaml
schema_version: 1
name: notifier
version: 0.1.0
description: Desktop notifications for jin sessions
license: MIT
homepage: https://github.com/foo/notifier
jin: ">=0.7.0"
install:
  source:
    build:
      - go build -o bin/notifier ./cmd/notifier
    entrypoint: ./bin/notifier
on: ["status_changed:idle", "status_changed:permission"]
timeout: 30s
```

| フィールド | 必須 | 説明 |
|-----------|------|------|
| `schema_version` | あり | マニフェスト世代。現行は `1`。破壊的変更時のみ bump |
| `name` | あり | `[a-z][a-z0-9-]{1,63}`。レジストリ内で unique。jind-ai がインストールするディレクトリ名と一致 |
| `version` | あり | プラグイン自身の semver（`X.Y.Z`。pre-release / build metadata 可） |
| `description` | あり | `jin plugin ls-remote` の検索結果に表示される一行説明 |
| `license` / `homepage` | なし | 任意メタデータ。レジストリエントリに載る |
| `jin` | あり | jin バイナリに対する semver 制約（`">=0.7.0"`、`"^0.7"`、`">=0.7 <0.9"`）。install 時と毎 dispatch 時にチェック |
| `install.source.build` | 条件付き | ビルドコマンド配列（各要素が独立の `bash -c`。要素をまたぐパイプは不可） — [言語別ガイド](#言語別ガイド) を参照。`install.source` 使用時は必須 |
| `install.source.entrypoint` | 条件付き | ディスパッチャーが毎イベント実行するパス（プラグインディレクトリ相対）。`install.source` 使用時は必須 |
| `install.release_asset.pattern` | 条件付き | `install.source` の代替。最新 GitHub Release から prebuilt asset をダウンロード。プレースホルダ: `{os}` / `{arch}` |
| `on` | なし | `status_changed` または `status_changed:<status>` マッチャーのリスト。空または省略時は action 専用 |
| `timeout` | なし | 期間文字列（`"30s"`、`"5m"`）。デフォルト `30s` |
| `popup.width` / `popup.height` | なし | `jin pane popup --here` のサイズヒント（1–100、%） |

`install.source` と `install.release_asset` は排他です。

`config.yaml` はプラグインの有効/無効切り替えと dispatch タイミングの調整（下記）のみを行います — マニフェストのフィールドを重複して持つことはありません。

### プラグインが受け取る情報

環境変数:

| 変数 | 説明 |
|------|------|
| `JIN_EVENT` | `status_changed` または `action` |
| `JIN_SESSION_ID` | セッション ID |
| `JIN_STATUS` | 現在のステータス |
| `JIN_PREV_STATUS` | 直前のステータス（`action` 実行時は空） |
| `JIN_AGENT_KIND` | アダプタの種類（`claude` など） |
| `JIN_WORKDIR` | セッションの作業ディレクトリ |
| `JIN_TMUX_PANE_ID` | tmux ペイン ID（判明している場合） |
| `JIN_NOTIFY_KIND` | この遷移の通知種別: `task-complete`、`error`、`permission`。通知を伴わない遷移では空 |
| `JIN_PLUGIN_DEPTH` | チェーンの深さ — [制約](#制約) を参照 |
| `JIN_SOCKET` | デーモンソケットのパス。プラグインが呼び出す `jin` CLI はこれを自動的に読み取ります |
| `JIN_BIN` | デーモン自身の `jin` バイナリの絶対パス。PATH 上の `jin` は新しいサブコマンドを持たない古いインストールである可能性があるため、素の `jin` より `"${JIN_BIN:-jin}"` を優先してください |
| `JIN_CALLER_TMUX_SOCKET` | action 実行時のみ: 呼び出し元 CLI がいた tmux サーバのソケットパス（呼び出し元の `$TMUX` 由来）。呼び出し元が tmux 外の場合は未設定（空文字ではない） |
| `JIN_CALLER_TMUX_PANE` | action 実行時のみ: 呼び出し元 CLI のペイン ID（`$TMUX_PANE` 由来）。不明な場合は未設定 |

同じデータは **stdin に JSON としても** 書き込まれます（フィールドは同一、snake_case。caller tmux コンテキストは環境変数のみ）。

この薄いペイロード以上の情報が必要な場合は、jind-ai に問い合わせます:

```bash
jin session info "$JIN_SESSION_ID" --json    # full session details
jin session send "$JIN_SESSION_ID" "..."     # send a prompt
jin session result "$JIN_SESSION_ID" --json  # structured transcript entries
jin session focus "$JIN_SESSION_ID"          # make the running TUI display this session
jin pane popup "$JIN_SESSION_ID" -- <cmd>    # tmux popup over the session's pane
jin pane popup --here -- <cmd>               # tmux popup over the caller's own pane (uses $TMUX, falling back to JIN_CALLER_TMUX_SOCKET)
jin pane split "$JIN_SESSION_ID" -- <cmd>
jin pane capture "$JIN_SESSION_ID"
jin pane send-keys "$JIN_SESSION_ID" <keys>
```

**互換性の契約**: 見覚えのない環境変数・JSON フィールド・CLI フラグはエラーではなく無視すべきものとして扱ってください。jind-ai は同一 `schema_version` 内でこのサーフェスへ追加することはあっても、削除・改名することはありません。破壊的削除は `schema_version` の bump（pre-1.0 では jin の minor リリース）でのみ発生します。

### インストール / 更新 / 削除 / 一覧

```bash
# From a git source (github.com/, gitlab.com/, self-hosted, ssh URLs, ...)
jin plugin install github.com/owner/repo          # default branch
jin plugin install github.com/owner/repo@v1.2.0   # pinned to a tag/branch/SHA

# From a local directory, symlinked in place (development)
jin plugin install --link ./my-plugin

jin plugin update <name>
jin plugin remove <name>
jin plugin list          # NAME / VERSION / STATE / SOURCE; --json for scripting
```

git からの install/update では、何かに触れる前にマニフェスト（`name`、`version`、`on`、`entrypoint`、`build`）と解決したコミット SHA を表示し、確認を求めます（`--yes` でスキップ可）。承認されたコミット SHA は `plugins.lock.yaml` に記録されるため、以降の `install`/`update` が確認時と異なるコミットへ黙って進むことはありません。`--link` したプラグインはこの確認をスキップします — ローカルパスをリンクすること自体が信頼の意思表示であり、jind-ai はリンクされたプラグインに対してビルドを実行しません。

### 言語別ガイド

- **Shell / 単一ファイル** — `install.source.build` を 1 要素にしてスクリプトをコピー / `chmod +x` する程度で済みます。`entrypoint` はそのスクリプトを指します。
- **Node.js / TypeScript** — `dist/`（esbuild 等）にバンドルするビルドステップを 1 つ書いてください。ランタイム依存解決（bun/deno）も動作しますが、dispatch は fail-open のため初回 dispatch 時のネットワーク取得が黙って失敗することがあります — 事前ビルド済みバンドルの方が予測可能です。
- **Go / Rust などのコンパイル言語** — `install.source.build` にビルド手順を宣言してください。各要素は独立プロセスとして実行され（要素をまたぐパイプは不可）、ユーザーのプラットフォーム/アーキテクチャに合わせたバイナリを生成できます（`go.sum` / `Cargo.lock` は再現性のために有用）。ビルドは install/update ごとに一度だけ実行されます。jind-ai は依存解決やツールチェーンの検出を代行しないため、必要なものはプラグイン自身の README に明記してください。非ゼロ終了した場合 install/update はアトミックに失敗し（中途半端な状態は残りません）、出力は `~/.local/state/jind-ai/plugin-logs/<name>-build.log` に保存されます。jind-ai はデフォルトでビルド環境に `npm_config_ignore_scripts=true` を注入します（サプライチェーン対策で、自分のビルドステップ内で上書き可能）。ビルド自体はサンドボックス化されておらず、ユーザー自身の権限で実行されます。

### 制約

- **永続プロセスは不可。** jind-ai はイベント/アクションごとにプラグインを起動し、終了後は破棄します。長時間稼働するデーモンを `entrypoint` に組み込まないでください。常駐プロセスが必要な場合は自分で起動し（手動、または systemd user unit として）、プラグイン自体はそこへの薄いクライアント（例: `curl`）にとどめてください。
- **ポップアップは `JIN_*` 環境変数を継承しません。** `jin pane popup` / `jin pane split` は tmux が新規に spawn したプロセスでコマンドを実行するため、ポップアップに必要なデータはコマンドライン引数として渡してください（またはコマンド文字列内の env 代入プレフィックスとして。例: `jin pane popup "$JIN_SESSION_ID" -- "JIN_BIN=$JIN_BIN inner.sh --id $JIN_SESSION_ID"`）。
- **Fail-open。** エラー・タイムアウト・ハングしたプラグインがセッションのステータスパイプラインをブロックすることはありません — ログに記録され、パイプラインは処理を続行します。タイムアウトのデフォルトは 30s（マニフェストの `timeout:`）。
- **ループの残存リスク。** jind-ai は同一の (plugin, session, event) の短時間内での繰り返し dispatch をデバウンスし（デフォルト 3s、下記の `plugins.debounce`）、プラグインが別のプラグイン実行を 1 ホップを超えて連鎖させることを拒否します（`JIN_PLUGIN_DEPTH`）。ただしどちらも *遅い* ピンポン（例: プラグインが送信したプロンプトへの応答が数秒後に同じプラグインを再トリガーする）は捕捉できません — これを避けるのはプラグイン作者の責任です。

### 設定（`~/.config/jind-ai/config.yaml`）

```yaml
plugins:
  enabled: true          # デフォルト true。false にすると全プラグインのディスパッチを無効化
  disabled: ["notifier"] # 個別プラグインを名前で無効化
  build_timeout: 300  # 秒。install/update 時のビルドステップ（デフォルト 300）
  debounce: 3          # 秒。ディスパッチのデバウンス窓（デフォルト 3）
```

### 互換性

プラグインは `jin:` に jin バイナリの semver 制約（例: `">=0.7.0"`、`"^0.7"`）を宣言します。チェックは install/update 時（fail-closed — 範囲外のプラグインは何も書き込まれる前に拒否されます）と、dispatch のたびに再度行われます（fail-open — 互換性のないインストール済みプラグインはスキップされ、一度だけログに記録され、`jin plugin list` で `incompatible` と表示されます。`jin plugin run` は `jin plugin update <name>` を促します）。開発ビルド（`jin --version` が `dev` や未 stamp を返す場合）は制約を無条件で満たしたものとして扱われるため、ローカルでのプラグイン作業を阻害しません。

`schema_version` フィールドは `jin` とは直交する概念で、マニフェスト世代を識別します。jind-ai は `[min, current]` のウィンドウで schema をサポートし、現行は両方とも `1` です。将来 bump が始まっても 2 世代前までは受け付けます。

### プラグインのデバッグ

```bash
export JIN_DEBUG=1
tail -f ~/.local/state/jind-ai/plugin-debug.log        # ディスパッチャーの判断ログ
tail -f ~/.local/state/jind-ai/plugin-logs/<name>.log  # プラグイン自身の stdout/stderr
```

## デバッグ

```bash
# デバッグログを有効化
export JIN_DEBUG=1

# デーモン起動
jin daemon start

# ログ確認
tail -f ~/.local/state/jind-ai/daemon-debug.log
```

## 必要要件

- Go 1.24.5+
- tmux 3.3+
- Claude Code CLI がインストールされていること

## ライセンス

MIT
