# 純正 Misskey との差分カタログ

Elythia が持つ「純正 Misskey (misskey-dev/misskey) には無い、または挙動が異なる」ものをまとめたリファレンスの目次。差分そのものは、領域ごとに [`docs/divergence/`](#領域ごとの差分) のファイルへ分けてある (#3414)。

- 基準: **Elythia 2.0.0** ⇔ Misskey TS `2026.10.0`
- 最終更新: 2026-10-07 (領域ごとのファイルへの分割、#3414)

> ベースラインを固定したのは 1.0.0 (= Misskey TS `2026.7.0` 追従完了時点)。1.1.x は
> upstream を追従したのではなく、**Elythia 側の独自変更と互換性 fix** を積んだもので、比較対象の
> Misskey TS は 1.0.0 時点と同じ `2026.7.0` のままだった。**2026.9.0 への追従 (#2877) で
> ベースラインを `2026.9.0` へ更新し、2026.9.1 への追従 (#3176) で `2026.9.1`、2026.10.0 への追従 (#3285) で `2026.10.0` へ上げた。**
> 個々の記述はまだ 2026.7.0 時点の観察に基づくものが
> 混じりうるので、乖離を判断するときは対象の実装を現 pin (`2026.10.0-mk.5`) で確認すること。

## このドキュメントの位置づけ

Elythia は Misskey TS からの drop-in 移行 (同じ DB / Redis / frontend を引き継ぎ、backend だけ差し替えられる) を最優先とする。TS へ戻せること (復路) は保証しない (#3191)。このドキュメントで、TS へ戻したときの挙動を理由にしている記述 (別テーブルにした理由、seed の gate など) は当時の判断の記録で、今は保証ではない。したがって「差分」は無条件に悪ではなく、次の 4 種類に分かれる。

| 分類 | 意味 | 扱い |
|---|---|---|
| **cherrypick 由来** | yojo-art/cherrypick 系列が純正に加えた拡張を Elythia が取り込んだもの | 維持。vanilla misskey-js golden で厳密 gate しない |
| **Elythia 独自** | Elythia が独自に足した機能 (additive、wire 互換を壊さない) | 維持 |
| **安全側 divergence** | upstream より厳しい / 正確な挙動 | 維持し理由を明記 (Elythia 優位は regress させず理由を明記する方針) |
| **未実装 / 欠落** | upstream にあって Elythia に無い | issue 化して解消する |
| **近似** | 意図も結末も upstream と同じだが、依存ライブラリや判定の実装が違うため、数値や内部の形までは一致しない | §9 に残差を実測値つきで記録する |

1.0.0 = Misskey 2026.7.0 追従完了。**ここで drop-in 互換をベースラインとして固定し、以降 frontend の独自進化を解禁する。** 本ドキュメントはその「固定したベースラインからの距離」の一覧であり、1.0.0 時点のスナップショットとして機能する。

以降 upstream を追従するときは、本ドキュメントとの差分が新たな divergence になる。追従手順は [upstream-catch-up.md](upstream-catch-up.md) を参照。

## サマリ

| 軸 | Elythia 独自 | cherrypick 由来 | 未実装 |
|---|---|---|---|
| API endpoint | GET variant 23 + alias 4 + 分割アップロード 4 + 承認制 7 + 絵文字の申請 10 + exact assignment lookup 2 + admin 観測 8 + 配送のブレーカー 1 + 消えたインスタンスの片付け 2 + 連合のルール 5 + IP 検索 3 + バブルゲームの対戦 10 | chat 15 | **0** |
| API レスポンスの additive field | 8 (`runtime` / `mkGoVersion` / `chunkedUpload` / `approvalRequiredForSignup` / `registrationClosed` / `signupApplicationForm` / `canRequestCustomEmojis` / `minimumUsernameLength`) | reversi packed game の `crc32` 等 | — |
| DB テーブル | 19 (+ bookkeeping 2) | 0 | 0 |
| DB カラム | 23 (+ 未使用の残存列 3) | 3 | 0 |
| ActivityPub | Ed25519 / RemoteStatsFetcher ほか | reversi 連合 / chat 連合 | — |
| config キー | 20 前後 | 0 | — |
| fork frontend の独自変更 | 143 tag (`2026.7.0-mk.0` ～ `2026.10.0-mk.5`) | — | — |

**upstream endpoint の未実装はゼロ** (coverage 100.0%、444/444)。DB schema も upstream の全テーブル・全共有カラムを superset で保持しており、逆方向の欠落は無い。

## 領域ごとの差分

本家との差は、領域ごとに次のファイルへ分けて記録している (#3414)。節の番号 (§1〜§9) は分ける前と同じ。

| ファイル | 節 | 内容 |
|---|---|---|
| [`divergence/api.md`](divergence/api.md) | ピン留めノート / §1 | API |
| [`divergence/db.md`](divergence/db.md) | §2 | DB schema |
| [`divergence/federation.md`](divergence/federation.md) | §3 | ActivityPub (連合) |
| [`divergence/config.md`](divergence/config.md) | §4 / §4-1 / §4-3 | 設定・streaming・job queue |
| [`divergence/frontend.md`](divergence/frontend.md) | §4-2 / §4-2b | frontend |
| [`divergence/operations.md`](divergence/operations.md) | §5 / §5.5 / §5.6 | 運用・性能 |
| [`divergence/security.md`](divergence/security.md) | §6 / §7 | セキュリティと安全側の差分 |
| [`divergence/approximations.md`](divergence/approximations.md) | §8 / §9 | 逆方向の差分と近似 |

## メンテナンス

- **API endpoint の差分**: `make apicompat` で `docs/api-compat.md` を自動生成する (DB / Redis 稼働が必要)。upstream 側の fastify 直登録 endpoint は `ApiServerService.ts` から自動抽出するので、本家の版を上げたときの追随漏れは起きない。
  生成には DB / Redis 稼働が必要なので、使い捨ての postgres / valkey を `docker run` で立てて `elythia dump-routes` を回す (compose を使うと本番 UDS の project へ合流する事故があるため使わない)
- **entity shape の差分**: `docs/shape-drift.md` の L0 / L2 / L3 gate が CI で自動検出する
- **DB schema / migration の drop-in 安全性**: 以下の gate が CI で強制する (詳細は [shape-drift.md](shape-drift.md))
  - `TestSchemaDrift_CreateOnlyColumns` — `CREATE TABLE` 内でしか定義されていない upstream 非存在カラム (TS 製 DB では生えない)
  - `TestMigrationSeed_CoversUpstream` — TypeORM `migrations` テーブルの seed 漏れ (TS 復帰時の再実行)
  - `TestMigrationIdempotency_RequiresIfExists` — DDL の `IF [NOT] EXISTS` 漏れ (drop-in で migration が dirty 停止)
  - `TestIndexNaming_NoNewUpstreamDuplicates` — upstream と同内容の index を別名で追加 (TS 製 DB で二重化)
- **本ドキュメントの件数**: `TestDivergenceDoc_*` 7 件が CI で強制する (§1-1 の内部整合と生成物との突き合わせ、§2-1 / §2-2 の実 schema との突き合わせ、§4-1 の streaming チャンネル、§4-2 の fork tag、目次と `docs/divergence/` のファイルの突き合わせ)。ゲートは目次と、目次がリンクする領域ごとのファイルを続けて読む。**領域のファイルを足したら目次の表に行を足す** (足さないと `TestDivergenceDoc_IndexListsEveryAreaFile` が落ちる)。別途 `TestAPICompatDoc_MatchesRouter` が §1-1 の突き合わせ先 (`docs/api-compat.md`) を router.go と照合し、**錨が腐らないこと**を担保する。
  - §1-1 は (a) 見出し・表・サマリの内部整合と、(b) **`docs/api-compat.md` (= `make apicompat` の生成物) との突き合わせ**。(a) だけでは 3 箇所が揃って同じだけ間違っている状態を通す (develop では §1-1 が 53、生成物が 49、真値が 58 だった。5 件のうち 4 件は生成物の側には載っていたので、突き合わせていれば気付けた、#2640)
  - §2-1 / §2-2 は**実 schema (migration + `golden_upstream_columns.json`) との突き合わせ**。件数だけでなく行の有無も見るので、テーブル・カラムを足して表を更新し忘れると落ちる (#2634)
  - §4-2 の fork frontend tag は冒頭サマリの件数・範囲・連番と突き合わせる。§4-2 は #3379 で凍結した記録なので、表とサマリはもう動かない
- **値レベルの差分**: `make diff-test` (Elythia ↔ TS の応答を値単位で diff)
- **本家 e2e に対する適合**: `make upstream-e2e` (Misskey 本家の `test/e2e/**` を無改変で Elythia に向けて実行)。**意図的な差分は `tests/upstream-e2e/known-divergences.json` に根拠付きで登録し、expected-failure として扱う。** skip ではないので、乖離が解消して通るようになったら逆に落ちて気付ける。本ドキュメントに載せた divergence のうち API 挙動に現れるものは、原則この一覧にも entry がある ([upstream-backend-e2e.md](upstream-backend-e2e.md))
- **コード内の divergence 注記**: `grep -rn "#2106 L" internal/` で全件を辿れる
- **upstream 追従時**: `docs/update/` に release ごとの diff doc を追加し、そこで確定した divergence を本ドキュメントへ反映する。golden の再生成 (`make shapecheck-gen`) と TypeORM seed の追加も必要 ([upstream-catch-up.md](upstream-catch-up.md))
- **frontend の変更**: #3379 以降は本体の `frontend/` を直接直し ([contributing.md](contributing.md))、純正と違える挙動なら [§4-2b](divergence/frontend.md#4-2b-frontend-の独自変更-3379-で取り込んだ後) に PR 番号で 1 行足す。§4-2 の tag は取り込む前に fork で積んだ記録 (凍結)

## 関連ドキュメント

- [`api-compat.md`](api-compat.md) — endpoint 突き合わせ matrix (自動生成)
- [`shape-drift.md`](shape-drift.md) — entity shape drift gate
- [`federation.md`](federation.md) — 連合実装の詳細
- [`configuration.md`](configuration.md) — 設定キー一覧
- [`migration-from-ts.md`](migration-from-ts.md) — TS からの移行手順
- [`upstream-catch-up.md`](upstream-catch-up.md) — upstream 追従の手順とチェックリスト
- [`upstream-backend-e2e.md`](upstream-backend-e2e.md) — 本家 backend e2e を Elythia に向けて回す基盤と、既知乖離の運用
- [`ci.md`](ci.md) — CI で回る項目と、落ちたときの切り分け
- [`update/`](update/) — upstream release ごとの差分 doc
