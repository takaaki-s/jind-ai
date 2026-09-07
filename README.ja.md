[English](README.md) | **日本語**

# jind-ai

**10 のエージェントセッションを、1 つの画面で。**

- 自分待ちのエージェントを逃さない
- セッションごとに独立した git worktree
- マシンを再起動しても続きから
- エージェントがエージェントを操作する

tmux の上で動くので、既存の設定はそのまま。SSH 越しでも同じ画面に戻れます。

![8 つのエージェントセッションが 1 つの一覧に並ぶ。カーソルを下へ動かすと下のペインが追随して各セッションを説明し、切り替えは起きない。許可待ちのセッションには印が付き、その許可ダイアログが右に開いている。](https://github.com/takaaki-s/jind-ai/releases/download/v0.10.0/demo.gif)

> **For AI agents（AI エージェントの方へ）。** jin のエージェント向けリファレンスは
> このファイルではなくバイナリに同梱されており、インストール済みのバージョンと必ず
> 一致します。`jin docs list` で topic の一覧、`jin docs show <name>` で本文が読め、
> どちらも daemon を必要としません。リポジトリしか手元にない場合、同じ文書は
> [`internal/agentdocs/docs/`](internal/agentdocs/docs/) にあります（英語）。まずは
> [orchestration](internal/agentdocs/docs/orchestration.md) から。

## インストール

### GitHub Releases からダウンロード

[Releases ページ](https://github.com/takaaki-s/jind-ai/releases)からお使いの OS/アーキテクチャに合ったバイナリをダウンロードしてください。

```bash
# 例: Linux amd64
curl -Lo jind-ai.tar.gz https://github.com/takaaki-s/jind-ai/releases/latest/download/jind-ai_0.7.0_linux_amd64.tar.gz
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

## できること

**どれが自分待ちか分かる。** 状態はエージェント自身が報告します（thinking / idle /
許可待ち）。画面を読んで推測しているのではありません。

**エージェント同士がぶつからない。** `--worktree` を付けると、セッション起動と同時に
git worktree とブランチを切ります。並列で走らせても作業ツリーを共有しません。

**いつでも戻れる。** セッションは daemon より、エージェントより、マシンの再起動より
長生きします。開き直せば会話は止まったところから続きます。

**移動が速い。** `Ctrl+]` でデタッチ。`M-f` でセッション名・ディレクトリ・ブランチ・
フリート・エージェント種別を横断するファジー検索が開きます。

**エージェントを混ぜられる。** Claude Code / Codex / opencode をセッション単位で
選び、同じ画面に並べられます。

**スクリプトから、あるいは別のエージェントから動かせる。** 作成・送信・待機・結果の
読み出しが、いずれも `--json` で行えます。

**拡張できる。** 状態変化やオンデマンドで任意のプラグインを実行できます。デスクトップ
通知は今すぐ入れられるものの一例です。

ロジックは全て daemon 側にあり、TUI は Unix socket 越しの薄いクライアントです。
別のフロントエンドから同じ IPC を叩けます（[アーキテクチャ](docs/architecture.md) /
[IPC プロトコル](docs/ipc-protocol.md)）。

それぞれの具体的な使い方は、下の「CLI コマンド」「設定」「TUI キーバインド」
「プラグイン」の各セクションを参照してください。プラグインを書く場合は
[docs/plugins.ja.md](docs/plugins.ja.md) を参照してください。

## プロジェクトの状態

jind-ai は個人プロジェクトです。開発者が自身の日常利用のために開発・メンテナンス
しており、他の方にも役立つことを期待して公開しています。以下を前提としてご利用
ください。

- **サポートの保証はありません。** Issue / Pull Request に返信できないことがあります。
- **1.0 未満です。** 設定・IPC・plugin manifest に対する破壊的変更が任意の
  リリースで入り得ます。
- **スコープは意図的に狭く保っています。** Issue / PR を作成する前に
  [CONTRIBUTING.ja.md](CONTRIBUTING.ja.md) をご覧ください。

再現手順つきのバグ報告が最も歓迎される貢献です。

## 対応エージェント

| Kind | CLI | 備考 |
|---|---|---|
| `claude` (デフォルト) | [Claude Code](https://claude.com/product/claude-code) 2.x | first-class サポート。`--session-id` / `--resume` と CC のネイティブ hook で状態追跡。 |
| `codex` | [OpenAI Codex CLI](https://github.com/openai/codex) 0.144+ | spawn ごとに `-c hooks.X=[...]` で hook を注入。初回のみ `/hooks` ダイアログでトラスト承認が必要 (詳細: [docs/gotchas.md](docs/gotchas.md#codex-adapter))。Codex には `--session-id` 相当がないため、session UUID は `SessionStart` hook で受け取って daemon 側に書き戻す。 |
| `opencode` | [opencode](https://github.com/sst/opencode) 1.17+ | **Experimental。** 状態通知は `jin` バイナリに埋め込んだ TypeScript plugin が担当する。plugin は jind-ai の state 配下に展開し、`OPENCODE_CONFIG_DIR` で opencode に読ませる。この env は検索パスへの**加算**なので `~/.config/opencode` は汚さない (詳細: [docs/gotchas.md](docs/gotchas.md#opencode-adapter))。外部の bun インストールは不要。opencode にも `--session-id` 相当がないため、resume 用 ID は plugin の `SessionStart` で受け取る。opencode 自身の会話は SQLite に入っているため、`jin session result` は都度 `opencode export --pure <session id>` を実行してその出力を読む。jind-ai 側には何も記録しないが、その代わり `opencode` が daemon の PATH 上にある必要がある。 |

Claude Code を first-class citizen としてサポートしています。他エージェントは
`internal/agent/<kind>/` にアダプタを追加することで拡張可能です。

セッションごとに adapter を選ぶ:

```bash
jin session new --agent codex --workdir ~/repos/myrepo
```

`~/.config/jind-ai/config.yaml` に `default_agent: codex` を書けば常時デフォルトを切り替えられる。

TUI の作成フォームには、adapter が2つ以上登録されている場合に **agent picker のステップ**が出る。↑↓ / j/k + Enter でセッションごとに選べる。初期選択は `--agent` > `default_agent` > `"claude"` の順で決まる。`jin ui --agent codex` を使えば、その TUI 起動中だけ Codex を初期選択にできる（config は書き換えない）。

```bash
jin ui --agent codex   # 一時的なデフォルト。TUI 終了で戻る
```

### model を指定する

`--model` でセッション単位に model を選べる:

```bash
jin session new --model opus                    # Claude Code
jin session new --agent opencode --model anthropic/claude-opus-4-5
```

値は agent 自身の CLI にそのまま渡されるので、**その CLI の書き方**で書く。
Claude Code なら alias か完全名、opencode なら `provider/model` 形式。
jind-ai は model 一覧との照合を行わない。agent 側も必ず弾くとは限らず、しかも
挙動が揃っていない。Claude Code は起動して pane の中で警告を出すが、
opencode は起動して**何も言わない**。
**いずれにせよ打ち間違えても jin は「稼働中」と報告する。**opencode では pane を
見ても分からないので、セッションではなく綴りのほうを確認すること。

指定はセッションに保存され、resume のたびに再適用される（daemon 再起動後も同じ）。
設定できるのは作成時のみで、config のデフォルトも TUI の picker も無い。

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
| `idle` | ○ | `Stop` hook / 30秒間 hook が来なければ fallback | 入力待ち |
| `stopped` | ■ | プロセス死亡検知 | 停止済み |

**`codex` セッションは `permission` になりません。** Codex が承認フックを上げるのは
承認経路に入った時点であり、それは「人間に尋ねた」ことと同じではありません。jin が
受け取るペイロードにはどちらであったかを示す情報がないため、このアダプタは
`thinking` にマップします。実際に承認待ちの codex セッションも `thinking` と表示され、
`jin session respond` では応答できず、タイマーで解除されることもありません。pane に
アタッチして操作してください。Claude Code と opencode は影響を受けません。

## CLI コマンド

### デーモン管理

```bash
jin daemon start   # デーモン起動
jin daemon stop    # デーモン停止
jin daemon status  # 状態確認
```

**どのデーモンに届くか。** `JIN_SOCKET` があればそれを、無ければ既定のパス
（`$XDG_RUNTIME_DIR/jind-ai/daemon.sock`、`XDG_RUNTIME_DIR` が無い環境では
`$TMPDIR/jind-ai-<uid>/daemon.sock` —— macOS はこちらが通常）を使います。jind-ai の管理外で起動した
tmux サーバーは fork 時の環境を保持し続けるため、そのペインから `jin` を叩くと
古いデーモンに届くことがあります（警告は出ません）。`JIN_SOCKET` を明示するか、
そのサーバーを立て直してください。jind-ai が開くペインは影響を受けません。
詳細は [docs/ipc-protocol.md](docs/ipc-protocol.md#which-daemon-a-command-reaches)。

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

# セッションにプロンプトを送信（idle のセッションのみ）
jin session send <session-name> "プロンプト"

# プロンプト待ちで止まったセッションに回答する（Claude Code セッション）
jin session respond <session-name> --option 1
jin session respond <session-name> --text "bun を使って"

# セッションが idle になるまで待機（デフォルトタイムアウト: 300秒）
jin session wait <session-name>
jin session wait <session-name> --timeout 600

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

### スクリプト / 別のエージェントから動かす

セッション系コマンドはすべて `--json` に対応しています。スクリプトからでも別の
エージェントからでも、セッションを作り、プロンプトを送り、落ち着くまで待ち、
子が実際に何をしたかを読み戻せます。

```bash
jin session new --workdir ~/repos/myrepo --json
jin session send my-session "go test ./... を実行して失敗を報告して" --wait-running
jin session wait my-session --until idle,permission --timeout 600
jin session result my-session --json      # ツール呼び出しと結果。エージェント自身のログ由来
```

省略しやすく、間違えると高くつくのが `--wait-running` です。付けない場合 `send` は
「入力欄に届いた」ところまでしか報告しないため、送信されずに残ったプロンプトでも
終了コードは 0 になり、続く `wait` は即座に返ります。

許可待ちで子が止まったときの対処、結果の差分取得、エージェント種別ごとの対応範囲、
各終了コードとその対処 —— これらはバイナリに同梱されており、ここからも読めます
（いずれも英語）。

| 内容 | 参照先 |
|---|---|
| 委任ループの全体像 | [`jin docs show orchestration`](internal/agentdocs/docs/orchestration.md) |
| 終了コードと対処 | [`jin docs show exit-codes`](internal/agentdocs/docs/exit-codes.md) |
| セッションの指定方法 | [`jin docs show selectors`](internal/agentdocs/docs/selectors.md) |
| 驚くことになる挙動 | [`jin docs show gotchas`](internal/agentdocs/docs/gotchas.md) |

`jin init` を使うと、これらを自分で読むようエージェントに教える skill を
インストールできます。

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
  search: ["M-f"]         # セッション切り替えピッカー（ファジー検索）を開く
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
  # outer tmux (jin-mgr) — プラグイン action 単位のトリガー（左右両ペイン）
  # デフォルト無し。プラグイン x action 単位でユーザーがオプトインする。
  # tmux `run-shell -b` (バックグラウンド、ペインに出力を出さない) 経由で
  # `jin plugin run <name> <action>` を発火するため、popup を開くかどうかは
  # action 側の責務（`jin pane popup --here` モデル準拠）。未インストールの
  # プラグインは silent skip（log 1 行）。core outer-tmux バインドとのキー
  # 衝突は warning のみ、tmux 後勝ちに従う。
  # outer-tmux 系キーは tmux 記法 (`M-n`, `C-f`) と "+" 記法 (`alt+n`,
  # `ctrl+f`) の両方を受け付け、読み込み時に tmux 記法へ正規化される。
  # ※ 0.7.x の `plugins.<name>.keys` 形式は 0.8.0 で無効化される（起動時
  # に WARN が 1 回出て binding が drop される）。下記の
  # `plugins.<name>.actions.<id>.keys` 形式へ手動で書き直すこと。
  plugins:
    # notifier:
    #   actions:
    #     default: { keys: ["M-n"] }              # 1 打鍵で default action
    #     send-dm: { keys: ["M-d", "C-M-d"] }     # 複数キー / 別 action
    # worktree-cleanup:
    #   actions:
    #     default: { keys: ["M-w"] }
    # journal:
    #   actions:
    #     default: { keys: ["ctrl+f"] }           # "+" 記法も可

# ポップアップサイズ (percent、1-100 の int)。省略した項目はデフォルト値
# (create/session_filter/action = 70-80) が使われます。全項目と経路は
# docs/tui-guide.md#popup-sizes を参照。
popups:
  create:         { width: 80, height: 80 }
  session_filter: { width: 70, height: 70 }
  help:           { width: 60, height: 60 }
  action:         { width: 70, height: 70 }
  confirm:        { width: 50, height: 50 }  # Kill/Delete の確認 (既定は 48x10 セル)
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
| `{name}` | worktree 名（例: `jin-abcd1234` / `--worktree-name` で指定した名前） |
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
| `M-f` | セッション切り替えを開く（ファジー検索ポップアップ）— 詳細は [アウター tmux — セッション切り替え](#アウター-tmux--セッション切り替え) |
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

### アウター tmux — セッション切り替え

`M-f`（Alt+f、デフォルト）でセッション切り替えピッカーを開きます。ファジー検索ポップ
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
「switch session」エントリがあるため、ショートカット未設定でも起動できます。

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
| `JIN_WORKTREE_BASE` | ベースブランチ名 —— worktree は `origin/<この名前>` から切られる |
| `JIN_SESSION_ID` | 作成中セッションの UUID |
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

プラグインはステータス変化時、あるいはオンデマンドで実行されます。セッションが
自分待ちになったときのデスクトップ通知、どこかへの投稿、実行できるものなら何でも
構いません。実体は jind-ai がセッションの文脈を環境変数に載せて起動する普通の
プログラムで、本体には何も組み込まれていません（内蔵の通知機能はプラグインに
置き換えて削除されました）。

```bash
jin plugin ls-remote                       # レジストリの一覧
jin plugin install jind-ai-notifier        # インストール
jin plugin list                            # インストール済みの一覧
jin plugin run <name>                      # オンデマンド実行
```

インストール時にはマニフェストと解決されたコミットを表示し、何かに触れる前に
確認を求めます。

- **[プラグインを書く / インストールする](docs/plugins.ja.md)** —— マニフェスト形式、
  プラグインが受け取るもの、言語別ガイド、制約、互換性
- **[レジストリへの公開](docs/plugin-registry.md)** —— 一覧に載せる手順（英語）

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

- Go 1.26+
- tmux 3.5+（3.3a はセッションに再アタッチできません。3.6a と 3.7a にはそれぞれ表示バグがあります — 詳細は [docs/gotchas.md](docs/gotchas.md) を参照）
- Claude Code CLI がインストールされていること

## ライセンス

MIT