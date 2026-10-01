# Drop-in e2e テスト (#364, Phase 13-1〜)

Misskey TS から mk-go への **drop-in 切替互換** を自動検証する e2e テスト基盤。
#362 で露呈した Redis キー名前空間違い / HTML→MFM 変換の取りこぼしのような
互換ギャップを、手動テストに頼らず CI / on-demand で再発検知することを目的とする。

## 構成

```
tests/dropin/
  gen-certs.sh                    # 自己署名証明書 (a, b 用)
  instance_a.yml / instance_b.yml # Misskey TS 設定
  instance_a_mk.yml               # mk-A に差し替えたときの mk-go 設定
  nginx_a.conf / nginx_b.conf     # SSL 前段
  conftest.py                     # pytest fixtures (tests/federation/common/ の Client を再利用)
  test_smoke.py                   # federation smoke
  run-swap-test.sh                # TS → mk-go → TS の orchestrator
  run-mkgo-born-test.sh           # mk-go 生まれの DB を TS に引き渡す orchestrator
  run-fedibird-test.sh            # Ed25519 双方向 verify の orchestrator
  test_swap_setup.py              # swap 前の seed
  test_swap_seed_mkgo_only.py     # mk-go 独自機能の残留データを作る
  test_swap_seed_ed25519_peer.py  # Ed25519 peer の seed
  test_swap_verify.py             # mk-A 上での state 引き継ぎ検証
  test_swap_roundtrip_verify.py   # TS-A へ戻したあとの検証
  test_mkgo_born_verify.py        # mk-go 生まれ DB を受けた TS の検証
  test_fedibird_ed25519.py        # Fedibird-like mock との verify
  fedibird_mock/                  # Ed25519 を expose する AP mock

docker-compose.dropin.yml           # TS-A / TS-B stack
docker-compose.dropin.mk.yml        # instance A を mk-go に差し替える overlay
docker-compose.dropin.fedibird.yml  # fedibird-like mock の overlay
```

### なぜ共通 harness が `tests/federation/common/` にあるのか

mk-go は既に `tests/federation/` で mk-go ↔ Misskey TS 連合テストを持っており、
そこに `MisskeyLikeClient` (httpx ベース) が整備済である。drop-in テストは
さらに TS ↔ TS (後続フェーズで TS ↔ mk) に広げるだけなので client は共有する。

## 実行方法

すべて docker compose 経由。ホストに pnpm / Python をインストールしない。

```bash
# 1. インスタンス起動 (初回は misskey/misskey image の pull で数分)
make dropin-up

# 2. smoke test 実行
make dropin-test

# 3. 片付け (volume まで削除)
make dropin-down

# (任意) ログ追跡
make dropin-logs
```

## Phase 進捗

- [x] Phase 13-1 (#365): TS ↔ TS 基盤 + smoke test
- [x] Phase 13-2 (#367): mk-go 差し替え overlay + swap シナリオ test
- [x] Phase 13-3 (#372): 機能マトリクス拡充 (visibility 種別 / userList / specified DM)
- [x] Phase 13-4 (#374): GitHub Actions 統合 (当時は nightly。#2291 で PR トリガーへ移行)

## Phase 13-2: mk-go 差し替え (drop-in swap)

`docker-compose.dropin.mk.yml` overlay と bash orchestrator
(`tests/dropin/run-swap-test.sh`) で「TS-A backend を mk-go に差し替えても DB /
Redis 上の state がそのまま引き継がれる」ことを e2e で検証する。

### 通常実行

```bash
# 完全自動の swap シナリオ test (推奨)
make dropin-swap-test

# orchestrator は以下を順次実行する (stage 番号はログの `===> stage N` と対応):
#   1.  TS-A + TS-B stack (+ fedibird mock) 起動 → healthy 待ち
#   2.  pytest test_swap_setup.py   (alice/bob/follow/baseline note)
#   3.  docker compose stop app-a   (TS-A backend 停止、DB / Redis は維持)
#   4.  overlay で app-a を mk-go ビルドに差し替えて起動
#   5.  mk-A healthy 待ち → nginx-a を再起動して upstream を張り替え
#   6.  pytest test_swap_verify.py  (timeline 残存、新規 reply / reaction の連合)
#   6b. pytest test_swap_seed_{mkgo_only,ed25519_peer}.py (残留データと peer を作る)
#   7.  mk-A backend 停止 (復路の準備)
#   8.  overlay を外して TS-A backend を起動 → healthy 待ち → nginx-a 張り替え
#   8d. TS が migration を再実行していないことを assert (#2244)
#   9.  pytest test_swap_roundtrip_verify.py (TS へ戻したあとの連合継続、#1082)
#   (終了時) trap の `===> cleanup` で撤去
```

**復路 (stage 7-9) まで含めて 1 本のシナリオ。** 「mk-go に移れる」だけでなく
「戻れる」ことまで見ないと drop-in とは言えない。stage 6b で mk-go 独自機能の行を
わざと残し、TS がそれを持ったまま起動・pack できるかを stage 9 で確かめる (#2372)。

### 手動運用 (デバッグ向け)

mk overlay を直接立ち上げて確認したい場合:

```bash
make dropin-mk-up      # base + overlay (= mk-A + TS-B)
make dropin-mk-test    # smoke test を mk-A に対して実行
make dropin-mk-down    # cleanup
make dropin-mk-logs    # ログ追跡
```

注意: `dropin-mk-up` は **clean DB** から mk-A を起動するので、TS-A→mk-A の
state 引き継ぎは検証されない。state 検証は `dropin-swap-test` 専用。

## Phase 13-3: 機能マトリクス (現状カバー範囲)

`make dropin-swap-test` の verify 段階で以下を検証する:

| シナリオ | テスト | 状態 |
|---------|--------|------|
| baseline note の home timeline 残存 | `test_post_swap_baseline_note_preserved` | pass |
| home visibility ノート残存 | `test_post_swap_home_visibility_preserved` | pass |
| followers visibility ノート残存 | `test_post_swap_followers_visibility_preserved` | pass |
| specified visibility (DM) 残存 | `test_post_swap_specified_note_preserved` | pass |
| user list メタデータ残存 | `test_post_swap_user_list_preserved` | pass |
| user list timeline 残存 (membership 間接検証) | `test_post_swap_user_list_timeline_preserved` | pass |
| mk-A から remote bob への reply 配信 | `test_post_swap_alice_can_reply` | pass |
| mk-A から remote bob への reaction 配信 | `test_post_swap_alice_can_react` | pass |
| mk-A の actor が Ed25519 を expose する | `test_post_swap_alice_actor_exposes_ed25519_assertion_method` | pass |
| 再取得しても Ed25519 鍵が変わらない | `test_post_swap_alice_actor_ed25519_stable_across_refetch` | pass |
| 参照先を失った引用が残る | `test_verify_dangling_quote_survived_swap` | pass |

**reply / reaction はもう xfail ではない** (#369 解消済み)。`test_swap_verify.py` に
xfail マーカーは 1 つも無く、すべて通常のアサートとして通る。

復路 (`test_swap_roundtrip_verify.py`) では以下を検証する。

| シナリオ | テスト |
|---|---|
| TS へ戻すと actor が `assertionMethod` を出さなくなる | `test_roundtrip_alice_actor_no_longer_exposes_assertion_method` |
| TS-A から投稿して bob が受け取れる | `test_roundtrip_alice_can_post_and_bob_receives` |
| bob が TS-A の note にリアクションできる | `test_roundtrip_bob_can_react_to_alice_note` |
| mk-go 独自の chat / reversi 行が残っていても TS が動く | `test_roundtrip_ts_survives_remote_{chat,reversi}_rows` |
| TS が alice の timeline を pack できる | `test_roundtrip_ts_can_still_pack_alice_timeline` |
| RSA へ戻した相手と mock が配送できる | `test_roundtrip_mock_can_still_deliver_with_rsa` |
| TS が Ed25519 peer を pack できる | `test_roundtrip_ts_can_pack_ed25519_peer` |
| TS が drive のファイルを列挙できる | `test_roundtrip_ts_can_list_drive_files` |

未カバー:

- カスタム絵文字 (display name / note 本文の :emoji:) — TS で emoji を作成する admin API 周りが必要
- WebSocket streaming (`/streaming` channel 接続後の note push 受信)
- channel timeline 残存

## トラブルシューティング

### `misskey/misskey:2026.10.0` が pull できない

`docker login` 等の認証不要。pull が失敗する場合は network / rate limit。
compose 内で pull を再試行するか `docker pull misskey/misskey:2026.10.0` で
先読みしておく。

### 連合 follow が timeout する

両 instance が互いに到達できる必要がある。`make dropin-logs` で nginx / app の
エラーを確認する。自己署名証明書のため app-a / app-b は
`NODE_TLS_REJECT_UNAUTHORIZED=0` で起動している。

### 残ったリソースが後続テストを汚染する

`make dropin-down` が volume まで削除する (`down -v`)。named volume `certs` も
同時に削除されるため、次回 `dropin-up` で再生成される。

## CI 実行

`.github/workflows/dropin-e2e.yml` が 5 シナリオを **matrix で並列**に実行する。

| check 名 | make target | 見ているもの |
|---|---|---|
| `swap-test` | `dropin-swap-test` | TS→mk-go 切替で state が保たれるか |
| `mkgo-born` | `dropin-mkgo-born-test` | mk-go 生まれの DB を TS に引き渡せるか |
| `ed25519-verify` | `dropin-fedibird-test` | Fedibird-like mock との Ed25519 双方向 verify |
| `federation` | `federation-misskey-e2e` | 本物の Misskey TS との実連合 |
| `federation-mastodon` | `federation-mastodon-e2e` | 本物の Mastodon との引用の承認 (FEP-044f) |

発火条件:

- **pull_request**: `internal/**` / `migration/**` / `tests/dropin/**` 等の paths に
  該当する変更のみ。ドキュメントだけの PR では回らない
- **workflow_dispatch**: 任意の ref を指定して手動実行

当初は nightly (schedule) だったが PR トリガーへ移行した (#2291)。nightly は失敗に
気付くのが翌日になるうえ、1 日分のマージがまとまってどの変更が壊したか特定しづらい。

`fail-fast: false` なので 1 つが落ちても他は完走する。これは重要で、この 4 つは実際に
**別々の壊れ方をする**。`ed25519-verify` は導入時から 2 箇所壊れていたが、`swap-test` が
緑だったため約 3 か月それに気付けなかった (#2360)。

PR の required check には**入れない**。理由:

- federation delivery の poll を含み若干 flaky で、merge ブロッカーには適さない
- 非ブロッキングを `continue-on-error` で実現しないこと。あれは job を成功扱いにする
  ので失敗が完全に不可視になる。job は正しく失敗させ、required に入れないことで
  非ブロッキングにする

失敗時は docker compose のログを `dropin-logs-<scenario>` artifact として 14 日保持する。
`swap-test` / `mkgo-born` は orchestrator 自身が `down -v` の前に `compose.log` /
`ps.log` を残しており、workflow が後から集めたものは `-post` 付きの別名で入る。
前者があればそちらが本命。

### `swap-test` と `mkgo-born` の違い

似て見えるが **DB を作った側が違う**。

|  | DB を作ったのは | 経路 |
|---|---|---|
| `swap-test` | TypeORM | TS → mk-go → TS |
| `mkgo-born` | **mk-go の migration** | mk-go → TS |

後者の方が厳しい。TS は一度も触っていない schema を受け取るので、カラム型・制約・
enum・index 名・default のどれかが TypeORM の期待とずれていれば起動しない。
`TestMigrationSeed_CoversUpstream` は seed 一覧と upstream migration file の
**静的な突き合わせ**に過ぎず、実際に TS を起動して確かめてはいない。

運用上これは**ロックインの有無そのもの**にあたる。「mk-go で始めた人が Misskey に
移れるか」に答えられるのはこの経路だけで、実際この経路の初回実行で、RSA 秘密鍵が
PKCS#1 のため TS 側の送信連合が全滅する不具合が見つかっている (#2380)。mk-go の
`ParseRSAPrivateKey` は PKCS#1 / PKCS#8 の両方を読めるため、**コードを読む限りでは
何も問題が無いように見える**類のバグだった。

`mkgo-born` が落ちた場合、段階から原因がほぼ特定できる。

| 落ちた段階 | 意味 |
|---|---|
| stage 4b (TS-A healthy 待ちで timeout) | mk-go の migration が作った schema を TypeORM が受け付けなかった |
| stage 4d (migrations digest 不一致) | TypeORM の `migrations` seed に漏れがあり TS が再実行した。追加する場所は **`000067`** (`ClassName + timestamp` 形式)。`000029` は短縮形で seed した初版で、`000067` がそれを本家と同じ形へ書き換えている |
| stage 5 (pytest) | schema は通ったがデータを読めない / 連合が続かない |
