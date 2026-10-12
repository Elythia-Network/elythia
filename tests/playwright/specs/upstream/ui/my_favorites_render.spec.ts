/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/favorites page で notes/favorites/create 後の note text が
// MkPagination + MkNote 経由で render されることを verify する spec。
//
// upstream Misskey の /my/favorites は i/favorites endpoint を Paginator
// で叩いて (favorite, note) を取得し、各 note を MkNote で render する。
// 本 spec は note text を hydration sign にする。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /my/favorites renders favorited notes', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('a favorited note appears in /my/favorites', async ({ page, baseURL, request }) => {
    // 自分の note を favorite する (= 一番シンプルに /my/favorites に乗る)
    const noteText = `pwfav-${Date.now().toString().slice(-9)}`;
    const noteResp = await callApi(request, 'notes/create', {
      i: root.token,
      text: noteText,
      visibility: 'public',
    });
    expect(noteResp.status()).toBe(200);
    const noteId = (await noteResp.json()).createdNote.id;

    const favResp = await callApi(request, 'notes/favorites/create', {
      i: root.token,
      noteId,
    });
    expect(favResp.status()).toBe(204);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/favorites`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      noteText,
      { timeout: 20_000 },
    );
  });
});
