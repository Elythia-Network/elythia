/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 開いたページが not-found でないことを確かめるヘルパ (#4025)。
//
// router は見つからないパスを not-found のページ (`/:(*)`) に回す。ナビや本文が
// 出るかだけを見ていると、存在しないパスを開いても緑になる (実際に 18 パスで
// そうなっていた)。
//
// **ページが mount するのを待ってから判定する。** 待たずに判定すると、not-found の
// 部品が出る前なので必ず通る。待ち方は次の順。
//   1. 起動画面 (`#splash`) が消える。アプリの mount の後に消える (boot/common.ts)
//   2. 通信が止む。ページの部品は遅延で読み込むので、その読み込みを待つ
// title の形 (`<title> | <instance>`) だけを待つと、サーバーが title を描くルート
// (`/@:acct`、`/notes/:id` など。internal/server/ssr_meta.go) では最初から
// その形なので、mount の前に通ってしまう。
//
// 判定は title で行う。not-found のページは mount すると `definePage` で title を
// i18n の `notFound` にする (spec は en-US で動かす。playwright.config.ts)。本文の
// 文 (`notFoundDescription`) では判定しない — 管理画面の branding が同じ文を
// 入力欄のラベルに使っている。

import { expect, type Page, type Response } from '@playwright/test';

export const NOT_FOUND_TITLE = 'Not found';

/** Waits for the routed page to mount and fails if it is the not-found page. */
export async function expectRoutedPage(page: Page, path: string, timeout = 20_000): Promise<void> {
  await page
    .waitForFunction(() => document.getElementById('splash') === null, undefined, { timeout })
    .catch((cause: unknown) => {
      throw new Error(`${path}: 起動画面が消えなかった (アプリが mount していない)`, { cause });
    });
  // ストリーミングの WebSocket は数えられないので、通信が止むこと自体は待てる。
  // 止まないときは時間切れで落とす (待たずに判定すると空振りするため)
  await page.waitForLoadState('networkidle', { timeout }).catch((cause: unknown) => {
    throw new Error(`${path}: 通信が止まず、ページの読み込みを待てなかった`, { cause });
  });
  const title = await page.title();
  expect(title.startsWith(`${NOT_FOUND_TITLE} | `) || title === NOT_FOUND_TITLE, `${path} は not-found のページになった (title: ${title})`).toBe(false);
}

/**
 * Opens `path` and fails unless it is served (200) and routed to a real page.
 * Route specs open pages through this so that the not-found check cannot be
 * forgotten (#4026). Returns the navigation response.
 */
export async function gotoRoute(page: Page, baseURL: string, path: string, timeout = 20_000): Promise<Response> {
  const resp = await page.goto(`${baseURL}${path}`, { waitUntil: 'domcontentloaded' });
  expect(resp, `goto ${path} returned null response`).not.toBeNull();
  // SPA は見つからないパスでも index.html を返すので、200 だけでは区別できない。
  // 区別は expectRoutedPage が受け持つ
  expect(resp!.status(), `${path} should return 200`).toBe(200);
  await expectRoutedPage(page, path, timeout);
  return resp!;
}
