# Drop-in frontend e2e テスト (#380, Phase 14-)

Misskey TS の実フロントエンドが期待する挙動を cypress で固定し、TS-A backend
を mk-go に差し替えた時も同じ挙動が得られるかを検証する e2e 基盤。

## 目的

- `pytest` ベースの Phase 13 e2e (`tests/dropin/`) は API レベルでの state
  preservation を確認するが、フロントエンド描画層のバグ (画像表示 / 削除反映 /
  emoji 描画 等) までは捕捉しにくい。
- cypress で実ブラウザ (electron) から TS フロントエンドを動かし、観測可能な
  挙動を regression test として固定する。
- 親 issue: #380、サブ: #381 (Phase 14-1、本ドキュメントの対象), #382〜 (Phase 14-2 以降)。

## 構成

```
  TS-C ─┐
        ├─ mutual follow + activities ─┐
  TS-B ─┤                              │
        └────────────────────┬─────────┤
                             ▼         ▼
                            TS-A → [swap, Phase 14-3] → mk-A
```

3 インスタンス (A / B / C) + 各々独立の Postgres / Redis / nginx + cypress runner。
baseline は 3 台とも TS。instance A を mk-go に差し替える overlay がある。

```
tests/dropin-frontend/
  gen-certs.sh                # a / b / c + bundle.pem 用自己署名証明書
  instance_a.yml              # TS 用 default.yml (A)
  instance_a_mk.yml           # A を mk-go に差し替えたときの設定
  instance_b.yml / instance_c.yml
  nginx_a.conf / nginx_b.conf / nginx_c.conf   # SSL 前段
  cypress/
    cypress.config.ts         # electron self-signed cert 許容
    tsconfig.json / package.json
    support/
      e2e.ts                  # uncaught 抑制
      api.ts                  # cy.request wrapper (createRootOrSignin, retryUntil 等)
      setup.ts                # 3 インスタンスの seed (setupTrio / establishFederation)
      mode.ts                 # CYPRESS_MODE による skip 制御 (skipInSwap)
    e2e/
      smoke.cy.ts
      visibility.cy.ts
      user_list.cy.ts
      cross_instance_view.cy.ts
      delete_note.cy.ts
      reply_chain.cy.ts
      federation_allowlist.cy.ts
  run-frontend-baseline.sh    # baseline orchestrator
  run-frontend-swap-test.sh   # TS-A → mk-A 切替の orchestrator

tests/dropin-frontend/compose.yml     # TS-A / TS-B / TS-C stack
tests/dropin-frontend/compose.mk.yml  # instance A を mk-go に差し替える overlay
```

## 実行

```bash
# baseline (3 TS のみ) 1 回走らせて cypress spec が全 pass することを確認
make dropin-frontend-baseline

# 手動で stack だけ上げて中に入りたいとき
make dropin-frontend-up
docker compose -f tests/dropin-frontend/compose.yml --profile test run --rm cypress-runner
make dropin-frontend-down

# ログ追跡
make dropin-frontend-logs
```

## Phase 進捗

- [x] Phase 14-1 (#381): 3 TS 基盤 + cypress smoke spec
- [x] Phase 14-2 (#387): spec マトリクス拡充 (visibility / userList / cross-instance / delete)
- [x] Phase 14-3 (#394): mk-go 差し替え overlay + swap orchestrator + nightly CI

## Phase 14-2 カバー spec

| spec | 内容 | 状態 |
|------|------|------|
| `smoke.cy.ts` | ping / webfinger / A follows B / C note → A | pass |
| `visibility.cy.ts` | public / home / followers / specified (DM) 4 種 | pass |
| `user_list.cy.ts` | alice が list 作成 + bob 追加 + list timeline 取得 | pass |
| `cross_instance_view.cy.ts` | charlie note を A/B 両方から observe して一致確認 | pass |
| `delete_note.cy.ts` | charlie 削除 → A/B timeline から消える | pass |
| `reply_chain.cy.ts` | charlie 起点の note への bob の reply が charlie に届く (1 hop) | pass |
| `federation_allowlist.cy.ts` | A の federation mode を `specified [B]` / `none` に切替えて A→B 通過 / A→C ブロックを検証 | pass (#536) |

attachment / emoji / reaction は Phase 14-2.5 以降 (admin emoji 投入フロー / 
remote image ingest (#378) / reaction deliver (#369) の条件整備が必要)。

## Phase 14-3: mk-go 差し替え overlay + swap orchestrator

`tests/dropin-frontend/compose.mk.yml` overlay + `tests/dropin-frontend/run-frontend-swap-test.sh` orchestrator で、**TS-A backend を mk-go に差し替えた後も cypress spec が pass する** ことを検証する。

### 実行

```bash
# 完全自動の swap シナリオ test (推奨)
make dropin-frontend-swap-test

# orchestrator の流れ:
#   1. TS-A / TS-B / TS-C stack 起動 (baseline)
#   2. CYPRESS_MODE=baseline で cypress run (7 ファイル / 15 test)
#   3. docker compose stop app-a (TS-A backend 停止、DB / Redis は維持)
#   4. overlay で app-a を mk-go に差し替えて起動
#   5. CYPRESS_MODE=swap で cypress run (skipInSwap の 4 test を除く 11 本)
#   6. teardown
```

### swap モードで skip される spec (既知のバグ)

| spec / test | skip 理由 |
|------------|-----------|
| `delete_note.cy.ts` 1 本 | mk-A が inbound Delete activity を fanout cache から purge しない (#379) |
| `user_list.cy.ts` 2 本 | `users/lists/push` が既 member 時に 500 を返す (#396)。2 本目は 1 本目の skip で `listId` が未設定になるため |
| `visibility.cy.ts` 1 本 | inbound specified Note が mk-A の mentions に現れない (#397) |

**`reply_chain.cy.ts` は skip していない。** queue back-pressure で flaky だった
2 hop 版 (#389) を **1 hop 版に書き換えて有効化済み**。AP の reply 配送は replyTo の
owner への直接デリバリなので、そこに届くことを見れば連鎖の整合は担保できる。

baseline (all TS) ではこれらも全 pass するため、skip は `CYPRESS_MODE=swap` 時のみ発動する。対応 issue 修正後に `skipInSwap` を外す。

### CI nightly

`.github/workflows/dropin-frontend-e2e.yml` で毎日 19:00 UTC (JST 04:00) に develop ブランチに対して `make dropin-frontend-swap-test` を実行する。失敗時は docker compose logs + cypress 成果物 (screenshots / videos) を `dropin-frontend-logs` artifact として 14 日保持する。

`workflow_dispatch` で mode 入力 (baseline / swap) を選択可能。

## トラブルシューティング

### cypress が self-signed cert を拒否する

`cypress.config.ts` で `setupNodeEvents` 内から `--ignore-certificate-errors`
を electron に渡している。spec 側は何もしなくて良い。

### `misskey/misskey:2026.10.0` の pull 失敗

`docker login` 不要。network / rate limit の可能性。`make dropin-frontend-up`
前に `docker pull misskey/misskey:2026.10.0` で先読みする手もある。

### cypress runner のログが流れない

`--rm` で使い捨て起動なので終了時にログが消える。persistent に確認したい時は
`--profile test run cypress-runner` (without `--rm`) で起動したあと
`docker logs <name>` で追う。

### 3 インスタンスが互いに到達できない

docker compose の `networks: [dropin_frontend]` で共有しているため通常は
自動解決する。`a` / `b` / `c` という alias で nginx が待ち受けるので、
federation URL は `https://a/`, `https://b/`, `https://c/` を使う。
