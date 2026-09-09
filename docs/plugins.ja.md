[English](plugins.md) | **日本語**

# プラグイン

jind-ai では、セッションのステータス変化に反応して、あるいはオンデマンドで、任意のシェル実行可能なプラグインを実行できます。プラグインはマニフェストとエントリーポイントのスクリプトを持つディレクトリです。jind-ai はスクリプトが何をするかには関与せず、いつ実行されどんな環境を受け取るかだけを管理します。

コミュニティプラグインは [plugin registry](plugin-registry.md) から発見できます。`jin plugin ls-remote` で一覧、`jin plugin install <name>` でレジストリ名指定インストール（コミット SHA ピン + 同意画面付き）が可能です。

## 3 通りの実行方式

- **Event listener（イベントリスナー）** — マニフェストの各 action がその `on:` マッチャー経由で `status_changed` を購読します。通知、ロギング、CI トリガーなど、非対話的な用途に向いています。注意: イベントはステータスが実際に変化した時のみ発火します。ステータス遷移を伴わない通知（既に idle の状態での再停止など）は dispatch されません。プラグインが複数の action を宣言している場合、それぞれ独立に match / debounce されるため、同一イベントで同じプラグイン内の複数 action が同時に fan-out することがあります。
- **Action（アクション）** — `jin plugin run <name> [action] [--session <selector>]` で明示的に起動します。ポップアップベースの diff レビュー UI のような、対話的なワークフローに向いています。`[action]` を省略するとプラグインの default action（`actions[0]`）が走り、action ID を渡すとその action を選択します。ある action の `on: []` を指定するとその action は action 専用になります。`--session` を省略すると **グローバル action** になり、セッション由来の環境変数はすべて空になります。action 実行時は (global・session 指定を問わず) 呼び出し元の CLI が tmux クライアント内にいた場合、`JIN_CALLER_TMUX_SOCKET` / `JIN_CALLER_TMUX_PANE` が起動元を示します。
- **PR handoff provider** — `handoff: true` の action を `jin session pr-handoff <session> <plugin> [action] --confirm` からだけ起動します。bounded evidence と idempotency key を受け取り、同期実行して bounded JSON を 1 件返します。`on` / `listener` / `popup` は併用できず、通常 action の UI には表示されません。`--dry-run` なら provider を起動せず core の preflight だけを実行します。

3 方式とも宣言された action の `entrypoint` を実行しますが、handoff provider は後述の厳密な stdin/stdout 契約に従います。

## マニフェスト（`jind-ai-plugin.yaml`）

このファイルをプラグインディレクトリのルートに配置します。ランタイム（ディスパッチャー）と publish 時（レジストリクローラー）が同じマニフェストを読みます。単一ファイル、単一の真実です。

```yaml
schema_version: 2
name: notifier
version: 0.2.0
description: Desktop notifications for jin sessions
license: MIT
homepage: https://github.com/foo/notifier
jin: ">=0.8.0"
timeout: 30s                                         # action 単位ではなくプラグイン単位
install:
  source:
    build:
      - go build -o bin/notifier ./cmd/notifier
actions:
  - id: default                                      # actions[0] が暗黙の default
    entrypoint: ./bin/notifier notify
    on: ["status_changed:idle", "status_changed:permission"]
    label: "Desktop notification"
  - id: send-dm                                      # `jin plugin run notifier send-dm`
    entrypoint: ./bin/notifier send-dm
    on: []                                           # action 専用（イベント購読なし）
    label: "Send DM to teammate"
    popup: { width: 60, height: 30 }
```

既存の v1 マニフェスト（`schema_version: 1` + `install.source.entrypoint` および top-level `on` / `timeout` / `popup`）はそのまま動きます。parse 時に単一 action 形式へ normalize されるため、プラグイン作者側の対応は不要です。新規は v2 で書いてください。

| フィールド | 必須 | 説明 |
|-----------|------|------|
| `schema_version` | あり | マニフェスト世代。`1` または `2`。v1 は parse 時に単一 action の v2 相当へ自動 normalize |
| `name` | あり | `[a-z][a-z0-9-]{1,63}`。レジストリ内で unique。jind-ai がインストールするディレクトリ名と一致 |
| `version` | あり | プラグイン自身の semver（`X.Y.Z`。pre-release / build metadata 可） |
| `description` | あり | `jin plugin ls-remote` の検索結果に表示される一行説明 |
| `license` / `homepage` | なし | 任意メタデータ。レジストリエントリに載る |
| `jin` | あり | jin バイナリに対する semver 制約（`">=0.8.0"`、`"^0.8"`、`">=0.8 <0.10"`）。install 時と毎 dispatch 時にチェック |
| `install.source.build` | なし | ビルドコマンド配列（各要素が独立の `bash -c`。要素をまたぐパイプは不可） — [言語別ガイド](#言語別ガイド) を参照。直接実行可能な entrypoint を同梱するプラグイン（shell script、リポジトリに commit した prebuilt バイナリ等）では省略可 |
| `install.source.entrypoint` | v1 のみ | ディスパッチャーが実行するデフォルト entrypoint。v2 では禁止（validate エラー）—— v2 は action ごとに entrypoint を宣言する |
| `install.release_asset.pattern` | 条件付き | `install.source` の代替。最新 GitHub Release から prebuilt asset をダウンロード。プレースホルダ: `{os}` / `{arch}` |
| `actions[]` | v2 のみ | プラグインが公開する action のリスト。`actions[0]` が暗黙の default。各要素が `id` / `entrypoint` / `on` / `label` / `popup` を持つ（以下の行を参照） |
| `actions[].id` | あり | `[a-z][a-z0-9-]{0,31}`（最大 32 文字。ID は CLI の引数トークンにもなるため `name` より意図的に狭い）。プラグイン内で unique。パレット / keybindings / `jin plugin run` すべてが ID で action を参照するため、明示 ID を強く推奨 |
| `actions[].entrypoint` | あり | この action の実行パス（プラグインディレクトリ相対）。すべての action が自前で持つ必要がある —— v2 にマニフェスト単位のフォールバックは無い |
| `actions[].on` | なし | この action の `status_changed` マッチャー。v1 の top-level `on` と同じ構文。空または省略時は action 専用。match / debounce は action 単位で独立 |
| `actions[].label` | なし | パレット / help popup に表示される人間可読ラベル。空の場合パレットは `<plugin>:<action-id>` を表示（default action の ID が `default` のときは plugin 名のみ） |
| `timeout`（top-level） | なし | このプラグインの action が実行できる時間。デフォルト `30s`。**action 単位の上書きは存在せず**、ディスパッチャーはこの 1 つの値を全 action に適用する |
| `actions[].popup.width` / `.height` | なし | `jin pane popup --here` の action 単位のサイズヒント（1–100、%） |
| `actions[].listener` | なし | この action を「イベント購読専用」とマーク。`on:` にマッチしたときは通常通り発火するが、ユーザー向けサーフェス（パレット / help popup / shell 補完）からは非表示になる。`jin plugin run <plugin> <action>` による直接起動は debug 目的で許可されたまま。`on:` 非空必須（listener with no events は無意味） |
| `actions[].handoff` | なし | 同期型の structured PR-handoff provider として宣言。通常 action の UI から隠れ、`jin session pr-handoff` からだけ実行可能。`on` は空で、`listener` / `popup` は指定不可 |
| `on` / `popup`（top-level） | v1 のみ | v1 レガシーフィールド。v2 では validate エラーになるため `actions[]` 側に書く。top-level の `timeout` は**この仲間ではなく**、v2 でも有効（上の行）|

`install.source` と `install.release_asset` は排他です。

`config.yaml` はプラグインの有効/無効切り替えと dispatch タイミングの調整（下記）のみを行います — マニフェストのフィールドを重複して持つことはありません。

**Listener action** はイベント購読と UI を両方持つプラグインの定番パターンです。責務を分けて、UI 側だけをパレットに露出させます:

```yaml
actions:
  - id: list                        # ユーザー向け: パレットに表示
    entrypoint: ./notifier.sh list
    label: "Show pending sessions"
  - id: listen                      # event listener: パレット非表示
    entrypoint: ./notifier.sh listen
    on: ["status_changed"]
    listener: true                  # on: 非空必須
```

## プラグインが受け取る情報

環境変数:

| 変数 | 説明 |
|------|------|
| `JIN_EVENT` | `status_changed`、`action`、または `pr_handoff` |
| `JIN_ACTION_ID` | この実行を発火させたマニフェスト action の ID（v1 マニフェストや v2 default action の合成時は `default`）。共通 entrypoint を書く場合、argv 分岐の代わりにこの env で action を識別できる |
| `JIN_SESSION_ID` | セッション ID |
| `JIN_STATUS` | 現在のステータス |
| `JIN_PREV_STATUS` | 直前のステータス（`action` 実行時は空） |
| `JIN_AGENT_KIND` | アダプタの種類（`claude` など） |
| `JIN_WORKDIR` | セッションの作業ディレクトリ |
| `JIN_TMUX_PANE_ID` | tmux ペイン ID（判明している場合） |
| `JIN_NOTIFY_KIND` | この遷移の通知種別: `task-complete`、`error`、`permission`。通知を伴わない遷移では空 |
| `JIN_PLUGIN_DEPTH` | チェーンの深さ — [制約](#制約) を参照 |
| `JIN_HANDOFF_KEY` | PR handoff 時のみ。stdin JSON にも含まれる安定した idempotency key |
| `JIN_SOCKET` | デーモンソケットのパス。プラグインが呼び出す `jin` CLI はこれを自動的に読み取ります |
| `JIN_BIN` | 稼働中のデーモンと一致する `jin` の絶対パス。jind-ai が state ディレクトリ配下に保持するコピーを指すため、デーモンの起動元バイナリが再ビルド・削除されても有効なままです。PATH 上の `jin` は新しいサブコマンドを持たない古いインストールである可能性があるため、素の `jin` より `"${JIN_BIN:-jin}"` を優先してください |
| `JIN_DEBUG` | デーモンがデバッグログ有効で動作している場合に `1`。プラグインが呼び戻す `jin` も自身の動作を記録します。無効時は `0` ではなく未設定 |
| `JIN_CALLER_TMUX_SOCKET` | action 実行時のみ: 呼び出し元 CLI がいた tmux サーバのソケットパス（呼び出し元の `$TMUX` 由来）。呼び出し元が tmux 外の場合は未設定（空文字ではない） |
| `JIN_CALLER_TMUX_PANE` | action 実行時のみ: 呼び出し元 CLI のペイン ID（`$TMUX_PANE` 由来）。不明な場合は未設定 |

同じデータは **stdin に JSON としても** 書き込まれます（フィールドは同一、snake_case。caller tmux コンテキストは環境変数のみ）。

### PR handoff provider 契約

handoff action は schema version 付き文書を受け取ります。内容は session ID、判明している場合の repository 名、base/head commit ID、branch、workspace fingerprint、bounded diff 集計、review 時刻、任意の current reported-check 結果です。path、ファイル名、patch本文、prompt、transcript、環境変数、credential は含みません。

stdout には JSON object をちょうど1件返します:

```json
{
  "status": "succeeded",
  "provider": "github",
  "id": "123",
  "url": "https://github.com/acme/app/pull/123"
}
```

`status` は `succeeded` または `failed` で、成功時は `id` か `url` が必須です。URL は credential、query、fragment を含まない HTTP(S) に限ります。stdout は32 KiB、各保存フィールドにはさらに小さい上限があります。診断は stderr に書いてください。timeout、非ゼロ終了、不正／過大な結果、daemon応答喪失はすべて外部結果が `unknown` です。provider は同じ idempotency key を再受信したら既存PRを照会するか同じ結果へ安全に収束し、重複PRを作らないよう実装する必要があります。

この薄いペイロード以上の情報が必要な場合は、jind-ai に問い合わせます:

```bash
jin session info "$JIN_SESSION_ID" --json    # セッションの詳細情報を取得
jin session send "$JIN_SESSION_ID" "..."     # プロンプトを送信
jin session result "$JIN_SESSION_ID" --json  # 構造化された transcript エントリを取得
jin session focus "$JIN_SESSION_ID"          # 起動中の TUI にこのセッションを表示させる
jin pane popup "$JIN_SESSION_ID" -- <cmd>    # セッションのペインに tmux popup を重ねる
jin pane popup --here -- <cmd>               # 呼び出し元自身のペインに tmux popup を重ねる（$TMUX を優先、無ければ JIN_CALLER_TMUX_SOCKET へフォールバック）
jin pane split "$JIN_SESSION_ID" -- <cmd>
jin pane capture "$JIN_SESSION_ID"
jin pane send-keys "$JIN_SESSION_ID" <keys>
```

**`jin pane popup` / `jin pane split` で開いたペインが受け取るもの**: `JIN_SOCKET`、`JIN_BIN`、`JIN_DEBUG`、`JIN_SESSION_ID` です。指すのは、あなたが呼び戻すよう伝えられたのと同じ jin と、そのペインの作業が属するセッションです。ポップアップや分割したペインの中からも、プラグイン本体と同じように（`"${JIN_BIN:-jin}"` を含めて）呼び戻せます。これらはどのペインにも渡ります — セレクタ指定でも `--here` でも、名前付きスロットを再起動した場合でも（`jin pane split --help` を参照）、コマンドを与えないシェルだけの分割でも同じです。jind-ai が知らない値は、省略ではなく空文字で渡されます。ペインの環境からキーを省くと、tmux がそのサーバの値で埋めてしまうためです。デバッグログが無効なときの `JIN_DEBUG` が、上記のように未設定ではなく空文字になるのもこの理由です。どちらの場合も `0` にはなりません。

ペインには `JIN_PLUGIN_DEPTH` も空で渡されます。これは identity の一部ではなく、「このペインはどのプラグインの連鎖も継続していない」という意味です。これがないと、何らかのプロセスが tmux サーバに残した深さがこのペインの呼び出し元のものとして読まれ、ここから実行する `jin plugin run` が黙って全て拒否されます。深さが実際に何を制限するのかは [制約](#制約) を参照してください。

**互換性の契約**: 見覚えのない環境変数・JSON フィールド・CLI フラグはエラーではなく無視すべきものとして扱ってください。jind-ai は同一 `schema_version` 内でこのサーフェスへ追加することはあっても、削除・改名することはありません。破壊的削除は `schema_version` の bump（pre-1.0 では jin の minor リリース）でのみ発生します。

## インストール / 更新 / 削除 / 一覧

```bash
# レジストリから (plugin-registry.md 参照)
jin plugin ls-remote                              # レジストリのプラグイン一覧
jin plugin install jind-ai-notifier               # latest release、以降 `plugin update` が追従
jin plugin install jind-ai-notifier -v 0.2.0      # バージョンピン、以降 `plugin update` は据え置き
jin plugin install jind-ai-notifier --force       # jin 互換範囲外でも強制インストール

# git ソースから (github.com/、gitlab.com/、self-hosted、ssh URL ...)
jin plugin install github.com/owner/repo          # default branch、以降 `plugin update` は最大 semver tag に追従
jin plugin install github.com/owner/repo@v1.2.0   # tag / branch / SHA でピン、以降 `plugin update` は据え置き

# ローカルディレクトリから（開発時、symlink）
jin plugin install --link ./my-plugin

jin plugin update <name>
jin plugin remove <name>
jin plugin list          # NAME / VERSION / STATE / SOURCE を表示。--json でスクリプト連携可

# マニフェストの validate — レジストリクローラーと同じチェック
jin plugin validate                               # デフォルトはカレントディレクトリ
jin plugin validate --github-actions              # ::error / ::warning annotation 形式で出力
```

git からの install/update では、何かに触れる前にマニフェスト（`name`、`version`、`on`、`entrypoint`、`build`）と解決したコミット SHA を表示し、確認を求めます（`--yes` でスキップ可）。承認されたコミット SHA は `plugins.lock.yaml` に記録されるため、以降の `install`/`update` が確認時と異なるコミットへ黙って進むことはありません。`--link` したプラグインはこの確認をスキップします — ローカルパスをリンクすること自体が信頼の意思表示であり、jind-ai はリンクされたプラグインに対してビルドを実行しません。

**`jin plugin update <name>` は unpin なインストールのみ latest release を再解決します**: registry 経由でインストールした plugin は registry の `latest_version` を、raw git URL でインストールした plugin は `git ls-remote --tags` で最大 semver tag を選択します（semver tag が無い場合は lock ref にフォールバック）。 `-v <ver>` (registry) や `@<ref>` (git URL) でピンして install した plugin は `plugin update` では動きません（据え置き旨のメッセージが出て終了、動かしたければ再 install してください）。install 時の「latest 追従 vs 据え置き」意図が lock に刻まれ、以降の update がそれを守ります。

## 言語別ガイド

- **Shell / 単一ファイル** — スクリプトを repo に commit し `entrypoint` から直接指して `install.source.build` は省略。スクリプトが生成物であるか実行権限が git 上で保持されない場合のみ、`chmod +x` の 1 要素を build に追加してください。
- **Node.js / TypeScript** — `dist/`（esbuild 等）にバンドルするビルドステップを 1 つ書いてください。ランタイム依存解決（bun/deno）も動作しますが、dispatch は fail-open のため初回 dispatch 時のネットワーク取得が黙って失敗することがあります — 事前ビルド済みバンドルの方が予測可能です。
- **Go / Rust などのコンパイル言語** — `install.source.build` にビルド手順を宣言してください。各要素は独立プロセスとして実行され（要素をまたぐパイプは不可）、ユーザーのプラットフォーム/アーキテクチャに合わせたバイナリを生成できます（`go.sum` / `Cargo.lock` は再現性のために有用）。ビルドは install/update ごとに一度だけ実行されます。jind-ai は依存解決やツールチェーンの検出を代行しないため、必要なものはプラグイン自身の README に明記してください。非ゼロ終了した場合 install/update はアトミックに失敗し（中途半端な状態は残りません）、出力は `~/.local/state/jind-ai/plugin-logs/<name>-build.log` に保存されます。制限時間はステップごとではなくシーケンス全体で 1 つです（`plugins.build_timeout`、既定 300 秒）。

  **ビルド環境は allowlist です。** ビルドステップに渡るのは、起動元プロセスから引き継いだ `PATH` / `HOME` / `USER` / `SHELL` / `LANG` / `TERM` / `LC_*` と、`npm_config_ignore_scripts=true`（サプライチェーン対策で、自分のビルドステップ内で上書き可能）だけです。`JAVA_HOME` や `CARGO_HOME`、レジストリのトークンなどが必要な場合は、継承を当てにせずビルドステップ内で導出してください。`jin plugin validate --run-build` は同じフィルタと同じ既定予算を適用し、あなた自身の `plugins.build_timeout`（この設定はプラグインを入れる側のものです）も manifest の `timeout:`（これは dispatch の応答時間であってコンパイル時間ではありません）も参照しません。したがって install が許す以上を必要とするビルドは、ユーザーの手元ではなくあなたの手元で落ちます。ただし通過した変数の**中身**までは保証できません。`PATH` はあなたのものなので、あなたにしか入っていないツールチェーンはローカルでは問題なくビルドできてしまいます。プラグインが必要とするものは自分の README に明記してください。ビルド自体はサンドボックス化されておらず、ユーザー自身の権限で実行されます。

## 制約

- **永続プロセスは不可。** jind-ai はイベント/アクションごとにプラグインを起動し、終了後は破棄します。長時間稼働するデーモンを `entrypoint` に組み込まないでください。常駐プロセスが必要な場合は自分で起動し（手動、または systemd user unit として）、プラグイン自体はそこへの薄いクライアント（例: `curl`）にとどめてください。
- **Fail-open。** エラー・タイムアウト・ハングしたプラグインがセッションのステータスパイプラインをブロックすることはありません — ログに記録され、パイプラインは処理を続行します。タイムアウトのデフォルトは 30s（マニフェストの `timeout:`）。
- **ループの残存リスク。** jind-ai は同一の (plugin, session, event) の短時間内での繰り返し dispatch をデバウンスし（デフォルト 3s、下記の `plugins.debounce`）、プラグインが別のプラグイン実行を 1 ホップを超えて連鎖させることを拒否します（`JIN_PLUGIN_DEPTH`）。ただしどちらも *遅い* ピンポン（例: プラグインが送信したプロンプトへの応答が数秒後に同じプラグインを再トリガーする）は捕捉できません — これを避けるのはプラグイン作者の責任です。また `jin pane popup` / `jin pane split` で開いたペインの中から始めた実行にも、どちらも届きません。深さはプラグイン自身の環境を伝わるものであり、ペインには開いた側の深さではなく空の `JIN_PLUGIN_DEPTH` が渡されるため、そこから始めた実行は再び深さ 1 から始まります。デバウンスの窓もステータス起点の dispatch を対象としており、`jin plugin run` は通りません。ポップアップから始める連鎖は無制限だと考えて、自分で止めてください。

## 設定（`~/.config/jind-ai/config.yaml`）

```yaml
plugins:
  enabled: true          # デフォルト true。false にすると全プラグインのディスパッチを無効化
  disabled: ["notifier"] # 個別プラグインを名前で無効化
  build_timeout: 300  # 秒。install/update のビルドシーケンス全体の予算（デフォルト 300）
  debounce: 3          # 秒。ディスパッチのデバウンス窓（デフォルト 3）
```

## 互換性

プラグインは `jin:` に jin バイナリの semver 制約（例: `">=0.7.0"`、`"^0.7"`）を宣言します。チェックは install/update 時（fail-closed — 範囲外のプラグインは何も書き込まれる前に拒否されます）と、dispatch のたびに再度行われます（fail-open — 互換性のないインストール済みプラグインはスキップされ、一度だけログに記録され、`jin plugin list` で `incompatible` と表示されます。`jin plugin run` は `jin plugin update <name>` を促します）。開発ビルド（`jin --version` が `dev` や未 stamp を返す場合）は制約を無条件で満たしたものとして扱われるため、ローカルでのプラグイン作業を阻害しません。

`schema_version` フィールドは `jin` とは直交する概念で、マニフェスト世代を識別します。jind-ai は `[min, current]` のウィンドウで schema をサポートし、現行は両方とも `1` です。将来 bump が始まっても 2 世代前までは受け付けます。

## プラグインのデバッグ

```bash
export JIN_DEBUG=1
tail -f ~/.local/state/jind-ai/plugin-debug.log        # ディスパッチャーの判断ログ
tail -f ~/.local/state/jind-ai/plugin-logs/<name>.log  # プラグイン自身の stdout/stderr
```

フラグは `JIN_DEBUG=1` としてプラグインにも届くため、プラグインが呼び戻した
`jin` も記録を残します — `jin hook` なら `hook-debug.log`、それ以外のデーモン側は
`daemon-debug.log` です。

---

[jind-ai](../README.ja.md) のドキュメントの一部です。
