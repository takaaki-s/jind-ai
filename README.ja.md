[English](README.md) | **日本語**

# jindaiko

複数の対話エージェントセッションを同時に稼働させ、一元管理するための CLI ツール
(Claude Code を first-class citizen としてサポート。他エージェントは
`internal/agent/<kind>/` にアダプタを追加することで拡張可能)。

https://github.com/user-attachments/assets/62e9d64a-aa7d-42f8-8edf-03f724fe0ee4

## 特長

- **複数セッション管理**: 複数の Claude Code セッションをバックグラウンドで同時実行
- **tmux ネイティブ**: セッションの実体は tmux 上で動く独立ペイン。普段お使いの `~/.tmux.conf` やカスタムキーバインド、ステータスバー、コピーモード等がそのまま使える
- **UI / ロジック分離アーキテクチャ**: セッション管理・状態遷移・hook 処理などのロジックは全て daemon に集約。TUI は Unix socket 経由で daemon を叩く薄いクライアントで、セッション管理ロジックを持たない。同じ IPC を叩けば TUI を別実装（Web UI・エディタ拡張等）に差し替えることも理論上可能（詳細は [docs/architecture.md](docs/architecture.md) / [docs/ipc-protocol.md](docs/ipc-protocol.md)）
- **TUI**: セッション一覧・状態確認・操作を対話的に行えるターミナル UI
- **アタッチ/デタッチ**: セッション間を素早く切り替え（`Ctrl+]` でデタッチ）
- **リアルタイム状態追跡**: 作業ディレクトリ・ブランチ・最新メッセージをリアルタイム表示
- **検索・ページング**: セッション名・ディレクトリ・ブランチでインクリメンタル検索
- **デスクトップ通知**: 許可待ち・タスク完了をOS通知でお知らせ（macOS / Linux対応）

## インストール

### GitHub Releases からダウンロード

[Releases ページ](https://github.com/takaaki-s/jindaiko/releases)からお使いの OS/アーキテクチャに合ったバイナリをダウンロードしてください。

```bash
# 例: Linux amd64
curl -Lo jindaiko.tar.gz https://github.com/takaaki-s/jindaiko/releases/latest/download/jindaiko_0.1.0_linux_amd64.tar.gz
tar xzf jindaiko.tar.gz
sudo mv jin /usr/local/bin/
```

### Go install

```bash
go install github.com/takaaki-s/jindaiko/cmd/jin@latest
```

### ソースからビルド

```bash
git clone https://github.com/takaaki-s/jindaiko.git
cd jindaiko
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
$XDG_CONFIG_HOME/jindaiko/      （デフォルト: ~/.config/jindaiko）
└── config.yaml                # 設定ファイル

$XDG_STATE_HOME/jindaiko/       （デフォルト: ~/.local/state/jindaiko）
├── state.yaml                 # 状態ファイル（前回使用したリポジトリ等）
├── sessions/                  # セッションデータ
├── hooks-settings.json        # Claude Code フック設定（自動生成）
├── daemon-debug.log           # デーモンデバッグログ（JIN_DEBUG=1 時）
└── hook-debug.log             # フックデバッグログ（JIN_DEBUG=1 時）

$XDG_RUNTIME_DIR/jindaiko/      （未設定時のフォールバック: $TMPDIR/jindaiko-<uid>）
└── daemon.sock                # デーモンソケット
```

### 設定例 (`~/.config/jindaiko/config.yaml`)

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
  search: ["/"]
  vscode: ["v"]
  notifications: ["!"]
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
```

### Worktree の作成先

`jin session new --worktree` はデフォルトで `$XDG_STATE_HOME/jindaiko/worktrees/{name}`（通常 `~/.local/state/jindaiko/worktrees/` 配下）に worktree を作成します。`config.yaml` の `worktree.base_dir` で任意の場所に変更できます:

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
| `/` | セッション検索（名前・ディレクトリ・ブランチ） |
| `Enter` | セッションにアタッチ |
| `n` | 新規セッション作成 |
| `x` | セッション終了 |
| `d` | セッション削除 |
| `r` | 一覧更新 |
| `v` | VS Codeで開く |
| `!` | 通知履歴 |
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

## Claude Code Hooks

jindaiko はセッションの状態検知に Claude Code の hooks を使用します。**Hooks は自動で設定されます** — 手動設定は不要です。

セッション起動時に jindaiko が `$XDG_STATE_HOME/jindaiko/hooks-settings.json`（デフォルト `~/.local/state/jindaiko/hooks-settings.json`）を生成し、`claude --settings` 経由で Claude Code に渡します。

各 hook の役割:

| Hook Event | 役割 |
|-----------|------|
| `UserPromptSubmit` | ユーザーがプロンプトを送信 → セッションを `thinking` に |
| `PostToolUse` | ツール実行完了 → セッションを `thinking` に（`permission` 状態からの復帰） |
| `Stop` | Claude のターン終了 → セッションを `idle` に（タスク完了通知を送信） |
| `Notification` | 権限要求等 → セッションを `permission` に（権限要求通知を送信） |

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

スクリプトはリポジトリにチェックインされる shell script なので、jindaiko は明示的に信頼されたリポジトリでない限り実行しません（direnv 流の allow モデル）。信頼はスクリプトの SHA256 で紐付けされ、スクリプトを編集すると再度信頼が必要になります。

```bash
jin worktree allow    # カレントリポジトリを信頼（スクリプト全文表示 + 確認プロンプト）
jin worktree revoke   # 信頼を取り消し
jin worktree status   # カレントリポジトリの信頼状態を表示
jin worktree list     # 信頼済みリポジトリを一覧
```

スクリプトが存在するが未信頼（または変更検知された）場合、hook は警告付きでスキップされ、worktree 自体は作成されて Claude は通常通り起動します。TUI からセッション作成した場合は popup 上で「許可する / スキップして作成 / やめる」の 3 択が表示されます。

### hook のスキップ

- `jin session new --worktree --no-hook` — このセッションだけ hook をスキップ
- `~/.config/jindaiko/config.yaml` に `worktree.hook_enabled: false` — 全リポジトリで hook を無効化
- `worktree.hook_timeout: <秒>` — タイムアウト変更（デフォルト `300`）。超過時はプロセスグループに `SIGTERM` を送り、5 秒の grace 後に生存していれば `SIGKILL`。

### 失敗時の挙動

hook が非ゼロ終了またはタイムアウトすると、worktree とブランチは rollback され、`jin session new` は非ゼロ exit code で失敗します。stdout/stderr は `~/.local/state/jindaiko/hook-logs/<session-id>.log` に保存され、rollback 後も診断のために残ります。

## デバッグ

```bash
# デバッグログを有効化
export JIN_DEBUG=1

# デーモン起動
jin daemon start

# ログ確認
tail -f ~/.local/state/jindaiko/daemon-debug.log
```

## 必要要件

- Go 1.21+
- tmux 3.3+
- Claude Code CLI がインストールされていること

## ライセンス

MIT
