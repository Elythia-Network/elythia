# Playwright e2e (#744)

mk-go 向けの Playwright e2e。spec は upstream Misskey TS の API 互換挙動を
期待値として書き、mk-go backend に対して走らせて drop-in 互換 regression を
検出する。

## 使い方

```bash
# stack 起動 (postgres / redis / mkgo)
make playwright-up

# Playwright runner で spec 実行
make playwright-test

# stack 停止 (volume 削除)
make playwright-down
```

## 構成

```
[Playwright runner] → [nginx (TLS)] → [mkgo] → [postgres / redis]
```

frontend asset を組み込んだ mk-go に対して実ブラウザから叩く。nginx は
self-signed cert で TLS を終端する (spec 側は `ignoreHTTPSErrors` で受ける)。

## upstream / mkgo の分割

```
specs/
├── upstream/       # upstream Misskey にも存在する機能の検証
│   ├── ui/         # ブラウザを駆動する (194 spec)
│   └── api/        # API の shape / 挙動 (96 spec)
└── mkgo/           # mk-go 独自機能の検証 (8 件)
```

### ui と api の境界

**判定は `page` fixture を使うかどうかで行う。** ブラウザを駆動するなら `ui/`、
使わないなら `api/`。

**`ui/` はブラウザを駆動する。** クリックや入力を伴うものだけでなく、`page.goto` して
HTTP status やリダイレクト先を見るだけの spec、`page.setContent` で iframe を張って
描画を確かめる spec も含む。画面が出せなければ落ちる以上、それは UI の検証にあたる。

**`api/` はブラウザを一切使わない。** `request` fixture で API を叩き、レスポンスの
shape や挙動を検証する spec。

分割前は `ui/` という名前のディレクトリに API 検証が混ざっており、**名前と中身が
一致していなかった**。「UI が壊れていないか」を知りたいときにどれを見ればよいか
分からない状態だったので、実態で分けた。

この判定条件は 2 度直している。いずれも「何をもって UI 検証と呼ぶか」を狭く取り
すぎたのが原因。

  - 分割時は「クリック等の操作があるか / 要素の表示を検証しているか」で振り分けた。
    **`page.goto` してレスポンスの status だけを見る spec がどちらにも当たらず**
    api 側へ落ち (70 件)、`upstream/api/ui/` という自己矛盾したパスが生まれた
  - 次に `page.goto` の有無へ改めたが、今度は **`uiSigninAsRoot()` のような helper
    経由で遷移する spec と、`page.setContent()` で iframe を張る spec** が漏れた
    (`post_note` / `embed` の 2 件)。どちらも実ブラウザでの操作そのものを見ている

`page` fixture を使うかどうかなら、遷移の書き方に依存しない。

この境界は「どちらが上等か」ではない。API の shape 検証は drop-in 互換の regression
検出に不可欠で、UI 操作より速く安定する。両方を別々に育てる。

**290 spec が `upstream/`、8 spec が `mkgo/`。** 分割時に全 spec を確認したが、mk-go 独自
機能 (cherrypick 由来の chat 拡張、`mkGoVersion` 等の additive field) を検証するものは
1 件も無かった。むしろ `i/profile_extra.spec.ts` のように **mk-go 拡張を明示的に scope
外としている** spec もある。

`mkgo/` の 8 件はいずれも公式 image では通らない。`ui/about_elythia.spec.ts` (#2700)
は mk-go 固有ページ `/about-elythia` を開く。`ui/boot_error_reload.spec.ts`
(#2786) は fork の `2026.7.0-mk.22c` で足した `#mkBootReload` を見る。
`ui/csp_enforce.spec.ts` (#2788) は mk-go 独自キー
`frontendContentSecurityPolicy` が返す CSP header を見るので、公式 image では
header 自体が無い。`ui/note_report_abuse_via_menu.spec.ts` と
`ui/user_report_abuse_via_menu.spec.ts` (#2879) は fork の `2026.9.0-mk.1` で入れた
通報の定型フォーム (`data-testid="abuse-report-details"` やカテゴリの `MkSelect`) を
見るので、公式 image では要素自体が無い。`ui/profile_moderation_note_button_align.spec.ts`
(#2926) はモデレーションノート追加ボタンの中央揃えを幾何で測るが、**純正はこれを
今も直していない**ので公式 image では左に寄ったまま落ちる。
`ui/federation_health_render.spec.ts` (#2944) は mk-go 独自の
`admin/federation/{delivery,inbox}-health` を叩くタブを見るので、公式 image では
endpoint 自体が無い。`ui/profile_avatar_lightbox.spec.ts` (#3124) は fork の
`2026.9.0-mk.38` で入れた「プロフィールのアイコンを押すと拡大表示する」を見るが、
**純正のアイコンは押せない素の画像**なので公式 image では何も開かない。
**`make playwright-ts-test` は `specs/upstream` に絞ってある**ので、TS backend 実行が
これで落ちることはない。

この境界には 2 つの意味がある。

  - **`upstream/` の spec は Misskey TS backend に対しても通る**。`make playwright-ts-test`
    と CI の `workflow_dispatch` 実行がそれを担保している。期待値そのものが upstream の
    実挙動と一致していることの検証になる
  - **本家へ還元しやすい**。`upstream/` の spec は本家にも存在する機能を検証しているので、
    本家の Playwright スイートへ持っていける

mk-go 独自機能の spec を書くときは `mkgo/` に置く。そちらは TS backend では通らないので、
`upstream/` に混ぜると `playwright-ts-test` が壊れる。

## ライセンスヘッダ

全 spec と fixtures に SPDX ヘッダを付けている。

```
/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */
```

mk-go リポジトリ自体が AGPL-3.0 なのでファイル単位のヘッダは必須ではないが、**由来を
明示する**ために入れている。本家の SPDX チェック
(`.github/workflows/check-spdx-license-id.yml`) は `SPDX-FileCopyrightText: syuilo and
misskey-project` **または** `SPDX-License-Identifier: AGPL-3.0-only` の OR 条件なので、
この表記のままでも本家 CI は通る。

## ディレクトリ構造

```
tests/playwright/
├── playwright.config.ts        # chromium のみ (`projects` は定義していない)
├── global-setup.ts             # Redis flush / root 確保 / registration open / quota purge
├── package.json                # @playwright/test 依存
├── Dockerfile.runner           # Playwright runner image
├── instance.yml                # mk-go config
├── nginx/                      # self-signed TLS を終端する reverse proxy
├── specs/
│   ├── upstream/ui/            # ブラウザを駆動する (194 spec)
│   ├── upstream/api/           # API の shape / 挙動 (96 spec)
│   └── mkgo/                   # mk-go 独自 (8 件)
└── fixtures/                   # 14 ファイル
    ├── api.ts                  # POST /api/<endpoint> ラッパ
    ├── auth.ts                 # signup / signin helper
    ├── ui_auth.ts              # ブラウザからのサインイン
    ├── quota.ts                # role policy 上限の後始末
    └── ...                     # abuse_report / backend / chat / files / notes / notifications / rate_limit / streaming / timeline / ui_click
```

## 並列度

**1 スタックに対しては直列で回すしかない** (`workers: 1`)。298 spec のうち 179 が
共有の root (alice) で**ブラウザからサインイン**し (数え方は
`grep -rlE 'uiSigninAsRoot|signin-username' specs --include='*.spec.ts' | wc -l`。
helper 経由が 178 で、`upstream/ui/signin.spec.ts` だけ signin フォームを直接駆動する)、
さらに 32 がサインインせず root の token で API を叩く (`root.json` を読むのが 211 で、
その差分)。instance meta は全 spec が共有する。Playwright は
ファイル単位で並列化するので、`workers` を上げると `profile_iscat_toggle` と
`profile_isbot_toggle` が同じアカウントを、`admin_branding_save` と
`about_page_render` が同じ meta を取り合う。root の quota
(antenna 5 / webhook 3 / clip 10) を消費する spec も 18 ある。

**並列度はスタックごと分けることでしか稼げない。** CI は `--shard=i/4` を 4 job に
分けて、それぞれ独立した stack を立てる (#2609)。

## design 原則

- spec は backend-agnostic (= TS / mk-go 両方で同 spec が pass するべき)
- 失敗 = 非互換 / regression として issue 化する運用
- pytest 版の drop-in e2e (`tests/dropin/`) / cypress 版 (`tests/dropin-frontend/`) は
  別系統として並走する。前者は TS ↔ mk-go の切替、後者は 3 TS インスタンスでの
  frontend 互換を見ており、守備範囲が重ならない
