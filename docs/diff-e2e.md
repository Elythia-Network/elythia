# Differential e2e diff harness (#2089)

mk-go と Misskey TS の**実 API レスポンスを並べて diff** し、静的コード比較や
entitycompat golden gate では拾えない**値レベルの乖離**を検出するハーネス。

## なぜ必要か

既存の互換性検証には次の層がある:

- **entitycompat golden gate**: error code/id/HTTP status と entity field の
  **presence (shape)** を golden と突き合わせる。
- **dropin / playwright e2e**: TS ↔ mk-go 切替が成立するか、フロントから見た
  挙動が壊れないかを検証する。

これらは「field が**存在するか**」「shape が合うか」は見るが、「field の**値**が
upstream と一致するか」(計算結果・正規化・順序・条件分岐の差) は検証しない。
本ハーネスはそこを埋める: **同一の入力に対する mk-go と TS の出力 JSON を field
単位で diff** し、instance 固有のノイズ (id/時刻/host 等) を除いた残差を parity
乖離の候補として報告する。

## アーキテクチャ

```
tests/diff/compose.yml   (隔離 stack、production UDS には触れない)
├─ mkgo  (build: tests/federation/common/Dockerfile.mkgo, config: tests/diff/mkgo.yml)
│   ├─ postgres-mk / redis-mk
├─ ts    (image: misskey/misskey:2026.10.0, config: tests/diff/ts.yml)
│   ├─ postgres-ts / redis-ts
└─ diff-runner (profiles:[test], pytest + requests)
     MKGO_URL=http://mkgo:3000  TS_URL=http://ts:3000
```

- **API-only (HTTP, no TLS/federation)**: runner は両 backend を `:3000` で直接
  叩く。WebAuthn/secure-context は不要なので nginx TLS 層は省く。
- **version**: mk-go・TS ともに **2026.10.0** で一致している。かつては公式 image が
  1 minor 遅れており version-gap のノイズを ignore-list で吸収していたが、その必要は
  無くなった。追従直後で公式 image が未公開の期間だけ、再び gap が生じうる。
- **隔離 (重要)**: compose 先頭で `name: mkdiff` を指定し専用 project に固定する。
  これが無いと default project は directory 名 `mk` になり、**本番 UDS stack
  (compose.uds.yaml, 同じ project `mk`) の `mkgo` サービスと衝突して本番コンテナを
  recreate してしまう** (整備中に実際に踏んで本番 mkgo を一時 clobber → 即復旧した)。
  `mkdiff-*` という別 namespace で own network/volumes を持ち、`make diff-down`
  (`down -v`) で完全破棄。本番 (UDS) には一切触れない。

- **one-shot**: seed は Misskey の bootstrap (admin/accounts/create が無認証で通るのは
  user 0 件のときだけ) を使うので、`make diff-test` は `make diff-up` 直後の fresh
  state に対して 1 回流す前提。再実行は `make diff-down && make diff-up` で作り直す。

## 使い方

```bash
make diff-up      # mkgo + ts (+ それぞれの DB/Redis) を build + 起動
make diff-test    # diff-runner (pytest) を両 instance に対して実行
make diff-logs    # ログ追尾
make diff-down    # stop + volume ごと破棄
```

`make diff-test` 失敗時、pytest の assertion message に `format_diffs` が
`[kind] $.path: mkgo=... ts=...` 形式で乖離を列挙する。

## 構成要素

| ファイル | 役割 |
|---|---|
| `tests/diff/diff_core.py` | JSON 値 diff の中核。ignore-list 付き再帰比較。**stdlib のみ**で unit-test 可能 |
| `tests/diff/test_diff_core.py` | diff-core の unit test (`python3 tests/diff/test_diff_core.py` で単体実行) |
| `tests/diff/conftest.py` | `mkgo` / `ts` fixture (Client) + health 待ち。env URL から接続 |
| `tests/diff/test_endpoints.py` | endpoint 別の差分テスト (35 件) |
| `tests/diff/{mkgo,ts}.yml` | 各 instance の config |
| `tests/diff/Dockerfile.runner` | pytest + requests の runner image |
| `tests/diff/compose.yml` | 2 backend + DB/Redis + runner |

## ignore-list 戦略

`diff_core.DEFAULT_IGNORE_KEYS` は 2 instance 間で必然的に異なる field を除外する
(id / createdAt / updatedAt / host / uri / url / token / publicKey / version 等)。
endpoint 固有のノイズ (meta の operator 設定、note の user オブジェクト等) は
各テストで `ignore_keys=` / `ignore_paths=` を足して吸収する。

方針は**保守的**: 「取りこぼし (本物の乖離を見逃す)」より「ノイズで埋もれて
本物が見えなくなる」方が害が大きいので、確実に instance 固有な field のみ無視し、
残差は人間が確認する。

## 検出した乖離の扱い

確認された値レベル乖離は **#2078 の sub-issue** として個別に起票し、通常の
通常の issue の流れ (CLAUDE.md Section 7) で潰す。
ハーネス自体の endpoint coverage 拡張は #2089 で追う。

## CI 方針

`.github/workflows/diff-e2e.yml` が **PR ごとに** `make diff-check` を実行する
(paths フィルタ付き、`workflow_dispatch` でも発火)。

dropin/playwright 同様、**required check には含めない** (2 backend 起動 +
image pull で重く、external image の flaky 要素もある)。CI 上の check 名は `diff`。

```bash
make diff-check    # down → up → healthy 待ち → pytest を通しで (クリーン DB 前提)
make diff-test     # スタックが既に上がっている場合
```

非ブロッキングを `continue-on-error` で実現しないこと (job が成功扱いになる)。

## カバレッジ

pytest の総数は 48 で、内訳は:

- **endpoint 比較 35 件** (`test_endpoints.py`) — mk-go と TS に同じリクエストを
  投げて値を突き合わせるもの
- diff-core の unit test 13 件 (`test_diff_core.py`) — 差分の取り方そのものの検証。
  stdlib のみなので `python3 tests/diff/test_diff_core.py` で単体実行できる

**「48 比較」ではない。** 実際に 2 backend を突き合わせているのは 35 件。

比較対象は meta / user (packing / rich profile / relation) / `i/me` /
note (packing / reaction / reply / renote / hashtag / state / poll) / clip / user list /
channel / antenna / drive file / drive folder / OAuth app / page / announcement /
emoji / flash / favorites / mute list / timeline (home / local / user notes /
followee) / locked follow request / **sinceId 単独指定のページング** (home
timeline / users/notes / drive folders / drive files / admin announcements)。

### sinceId 単独指定のページング (#2765)

cursor ページングはフロントの「もっと新しいものを読む」(`fetchNewer`) の中核で、
upstream の `makePaginationQuery` は **`sinceId` / `sinceDate` 単独のときだけ ASC**
で返す。mk-go は `internal/repository/pagination.go` の `paginationOrder` で同じ
規則を持つ (#2713 / PR #2764。数え方で 9 とも 12 とも書かれるが、**実際に
向きが変わったのは repository 関数 9 つ**で、残りは据え置き 1
(`emoji.go ListRemoteWithFilter` は upstream 自体が DESC) と挙動不変の helper
集約 2)。向きを逆にすると
「2 ページ目がおかしい」という形で利用者に出る。

**元から無防備だったわけではない。** 本家 backend e2e の
`.cache/misskey/<版>/packages/backend/test/e2e/timelines.ts` (`make upstream-fetch` で取得) が `users/notes` の
`sinceId` 単独 (ASC) と `sinceId` + `untilId` (DESC) を `deepStrictEqual` で
リテラル配列に固定しており、これは mk-go に対しても実行されている (vitest の
exclude にも `known-divergences.json` にも入っていない。`describe.each` の
FTT on/off で計 4 実行)。

**ただし守られていたのは `users/notes` だけ。** `clips.ts` も `users/clips` /
`clips/notes` に `sinceId` を投げるが、3 箇所とも
`res.sort(compareBy(s => s.id))` で**両辺を並べ替えてから**比較しており、
集合しか見ていない (順序回帰は落ちない)。

無かったのは **mk-go 側で管理するゲート**で、`tests/` / `test/` を横断して
`sinceId` を grep すると 0 件だった。しかも **#2713 が実際に直した経路
(drive folder / note draft / abuse report / chat / invite / reversi) は
1 つも入っていなかった**。

選んだ 5 経路と理由:

| endpoint | repository | 選んだ理由 |
|---|---|---|
| `notes/timeline` | `note.go ListHomeTimeline` | fanout 経由 (`sinceId` 付きは #2720 で必ず DB へ倒れる)。実利用が最も多い。**`meta.enableFanoutTimelineDbFallback` が off だと空が返る** (#2762、§5.6 参照) ので、そのときは `got=[]` で落ちる |
| `users/notes` | `note.go ListByUserIDFiltered` | fanout を通らない直行経路。本家 e2e も見ているが、あちらは mk-go 単体の assert で値の突き合わせはしない |
| `drive/folders` | `drive_folder.go ListByUser` | **#2764 が実際に直した経路**。note 系は元から ASC だったので、そこだけ見ても #2713 の回帰は捕まらない (mock 側は #2764 で `SortMockPage` に揃っているので、こちらは単体テストでも見える) |
| `drive/files` | `drive_file.go ListByUser` | 追加時点では **mock (`MockDriveFileRepository.ListByUser`) が sinceID 単独の ASC を実装しておらず**、単体テストからは順序回帰が見えなかった。#2766 で揃えたので今は mock でも見えるが、mock は production の SQL を実行しないので実 API 側のゲートは残す。frontend の MkDrive が `sinceId: '0'` で読む。**`sort` は渡さない** — production も upstream も sort 指定時は `paginationOrder` を通らず固定 order を使い、MkDrive も `-createdAt` のとき sort を送らない |
| `admin/announcements/list` | `announcement.go ListForAdmin` | note / drive 以外の repository |

**候補行数 > limit で読む。** 候補 <= limit だと `ORDER BY id ASC` と
`ORDER BY id DESC` が同じ行を返してしまい、「SQL は DESC のまま Go 側で slice を
reverse する」実装を素通しする。それは順序は合うが**返す行の集合が違う**
(最古 n 件ではなく最新 n 件) ので、ページに穴が空く。実運用のリクエストも
この形で、paginator の `fetchNewer` は limit 30 を投げる。

各テストは diff (mk-go と TS が一致するか) に加えて**向きそのものを直接
assert する**。diff だけだと「両方 DESC」でも通ってしまい、TS 側の実装に
依存してしまうため。

`test_meta_value_parity` の `META_IGNORE` は instance state (mediaProxy host /
proxyAccountName) のノイズを吸収するためのもの。version-gap 由来の除外は TS image を
2026.9.0 に揃えた時点で不要になったため、**残っているものが instance state 起因だけかを
追従のたびに見直すこと**。

## 既知の制約・今後

- 公式 image の公開は upstream release から遅れる。追従直後に version を厳密に
  合わせたい場合は、`make upstream-fetch` で取得した `.cache/misskey/<版>` からの source build に
  切り替える。
- endpoint を足すときは ignore-path の調整 (endpoint 固有ノイズの洗い出し) が
  伴う。**ignore-list を安易に広げないこと** — 空振りさせると本物の乖離が埋もれる。
  追加時は `docs/divergence.md` に対応する記述があるかを確認する。
  `META_IGNORE` と `USER_IGNORE` は別定義で後者は前者を継承していないので、
  `policies` のような両方に現れるキーは両方へ足す必要がある。
- auth が要る endpoint は `Client.ensure_admin` の token を使う。複雑な seed
  (follow/反応/連合) が要る endpoint は fixture を足して対応する。
