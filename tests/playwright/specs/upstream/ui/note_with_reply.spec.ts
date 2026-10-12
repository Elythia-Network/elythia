/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// note 詳細 page を reply chain がある状態で render → API で
// repliesCount=1 を verify する mixed (UI navigation + API write) e2e。
//
// 当初は UI で reply ボタン click → form fill → submit を狙ったが、upstream
// Misskey の note footer の reply icon に data-cy-* selector が無く programmatic
// 操作の起点が無いため、Vue 内部 class に依存するアプローチは fragility が高い。
// 妥協として:
//   - reply 自体は API で作成 (= notes/create with replyId)
//   - UI 側は親 note の /notes/:id を開いて hydration を verify
//   - chain 形成は API (notes/show.repliesCount) で verify
// reply 操作の真の UI e2e は upstream に reply 用 data-cy が追加されるまで保留。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: note detail page with reply chain', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(90_000);

  test('open note detail → API-side create reply → /notes/:id renders + API confirms chain', async ({ page, baseURL, request }) => {
    // 親 note を API で作成 (= UI 経由の post_note は別 spec で cover、本 spec
    // は reply chain の形成に focus)
    const parentText = `playwright-reply-parent ${Date.now()}`;
    const parent = await callApi(request, 'notes/create', {
      i: root.token,
      text: parentText,
      visibility: 'public',
    });
    expect(parent.status()).toBe(200);
    const parentBody = await parent.json();
    const parentId = parentBody.createdNote.id;

    // signin して /notes/:id を開く (= 認証済 で reply ボタン有効)
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/notes/${parentId}`, { waitUntil: 'domcontentloaded' });

    // 親 note の text が render されてから reply ボタンが visible になる
    await page.waitForFunction(
      (text) => document.body.textContent?.includes(text) ?? false,
      parentText,
      { timeout: 20_000 },
    );

    // reply 自体は API で作成 (= 本 spec scope は chain 形成 + 親 note hydration)
    const replyText = `playwright-reply-child ${Date.now()}`;
    const reply = await callApi(request, 'notes/create', {
      i: root.token,
      text: replyText,
      replyId: parentId,
      visibility: 'public',
    });
    expect(reply.status()).toBe(200);

    // reply chain が backend で形成されたことを API 経由で verify
    const showResp = await callApi(request, 'notes/show', { i: root.token, noteId: parentId });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    expect(shown.repliesCount, 'parent.repliesCount should be 1 after reply').toBe(1);
  });
});
