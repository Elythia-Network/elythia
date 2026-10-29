/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { defineConfig } from '@playwright/test';

// mk-go backend に対する Playwright e2e の設定 (#744)。spec は
// `specs/upstream/` に 290 ファイル、`specs/elythia/` に 8 ファイル。CI では `--shard=i/4` を 4 job に
// 分けて回す (docs/playwright.md)。
export default defineConfig({
  testDir: './specs',
  // globalSetup: Redis を flush して rate limit counter をゼロから始める
  // (#744 PR-2)。signup endpoint の 1h 5 回制限が test 累積で 429 になる
  // のを防ぐ。詳細は global-setup.ts のコメント参照。
  globalSetup: './global-setup.ts',
  // 各 spec はタイムアウト 30s を上限。signup / signin など API レイテンシ
  // しか伴わないので余裕を持たせて 30s で十分。
  timeout: 30_000,
  expect: {
    timeout: 10_000,
  },
  // list は人間向け、json は CI の "spec が 1 件も実行されなかった" guard
  // (.github/workflows/playwright.yml) 向け。outputFile は runner の
  // working_dir (/work = host の tests/playwright) 相対なので、host 側の
  // tests/playwright/results.json に落ちる。guard がこのパスを見るので
  // 両者を変えるときは必ず揃えること (#2276: reporter だけ入れ忘れて
  // guard が恒常 fail していた)。
  reporter: [['list'], ['json', { outputFile: 'results.json' }]],
  // chromium のみで 1 回ずつ実行する。`projects` は定義していない。検証対象は
  // mk-go backend の API 互換であってブラウザ差ではないので、multi-browser 化は
  // spec 数 x ブラウザ数だけ CI 時間を増やす割に得るものが少ない。
  use: {
    baseURL: process.env.MK_BASE_URL ?? 'https://mkgo.local',
    // 失敗の調査にしか使わないので、成功 run では取らない。
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
    // **録画しない。** 以前は 'on' で全 spec を録画していたが、CI では
    // 成功 run の成果物を一切アップロードしない (`if: failure() ||
    // cancelled()`) ので、録画したものをそのまま捨てていた。失敗 run でも
    // 実測で webm 256 本 40MB のうち失敗に対応するのは 2 本だけで、残りは
    // 成功した spec のもの。
    //
    // 'retain-on-failure' は全 spec を録画してから成功分を消す方式なので
    // 録画コストが残る。落とすなら 'off'。
    //
    // 失敗時の調査材料は trace が担う。trace には操作ごとの DOM スナップ
    // ショット・ネットワーク・コンソールが入っており、#2600 の flaky 調査で
    // 原因を特定したのも trace (「click したが /api/following/create が
    // 一度も呼ばれていない」) で、動画では判断できなかった。
    //
    // ローカルで動画が欲しいときは `--video=on` を渡す (#2609)。
    video: 'off',
    // nginx tls proxy が self-signed cert を提供する (#817 part2)。
    // Playwright はそれを accept できる必要がある。`request` fixture と
    // `page` fixture の両方に効く。
    ignoreHTTPSErrors: true,
    // i18n 依存 selector (button textContent === 'Save' 等) を持つ spec が
    // batch 5 で多数生まれた (PR #974)。CI runner / 開発者ローカル の OS
    // locale で日本語 UI に切り替わると一斉に fail するので、browser locale
    // を en-US に固定する。Misskey TS frontend は browser locale を
    // i18n key 解決に使うので、これで全 spec の text 前提が安定する。
    locale: 'en-US',
  },
  // **1 スタックに対しては直列で回すしかない。** 298 spec のうち 179 が共有の
  // root (alice) でブラウザからサインインし (数え方は
  // `grep -rlE 'uiSigninAsRoot|signin-username' tests/playwright/specs --include='*.spec.ts' | wc -l`)、
  // さらに 32 がサインインせず root の token で API を叩く
  // (`root.json` を読むのが 211 で、その差分)。
  // instance meta は全 spec が共有する。Playwright は
  // ファイル単位で並列化するので、workers を上げると `profile_iscat_toggle` と
  // `profile_isbot_toggle` が同じアカウントを、`admin_branding_save` と
  // `about_page_render` が同じ meta を取り合う。root の quota
  // (antenna 5 / webhook 3 / clip 10) を消費する spec も 18 ある。
  // 並列度は CI 側で `--shard=i/4` を 4 job に分けて稼ぐ (#2609)。
  workers: 1,
  // 1 度だけ retry を許可する。Chromium headless が稀に SIGSEGV (= signal 11
  // / GPF) で即死する infrastructure flake (= profile_iscat_toggle 1ms 即死
  // 例) と、SPA hydration race / WaitForResponse の short-window timeout を
  // retry で救う。spec の根本 bug を見落とさないよう、retries は **1 まで**
  // に絞る。fail/pass 切替の flaky を許容するわけではなく、`Test results:`
  // summary で `flaky` が出たら spec 側を直す対象とする。
  retries: 1,
});
