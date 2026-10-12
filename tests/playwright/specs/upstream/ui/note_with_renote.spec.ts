/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// note 詳細 page を renote chain がある状態で render → API で renoteCount=1
// を verify する mixed (UI navigation + API write) e2e。
//
// note_with_reply.spec.ts と同 pattern (= reply ボタンに data-cy 不在の問題は
// renote ボタンも同様)。renote 自体は API で作成し、UI 側は親 note の
// /notes/:id を開いて hydration を確認、count は API で verify。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, signupUser } from '../../../fixtures/auth';
import type { RootFixture } from '../../../fixtures/ui_auth';

test.describe('UI: note detail page with renote chain', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(30_000);

  test('open note detail → API-side create renote → /notes/:id renders + API confirms chain', async ({ page, baseURL, request }) => {
    // 親 note を作成
    const parentText = `playwright-renote-parent ${Date.now()}`;
    const parent = await callApi(request, 'notes/create', {
      i: root.token,
      text: parentText,
      visibility: 'public',
    });
    expect(parent.status()).toBe(200);
    const parentBody = await parent.json();
    const parentId = parentBody.createdNote.id;

    // renote を API で作成 (= notes/create with renoteId、本文は不要)。
    // 自分の note を自分で renote すると upstream は renoteCount を増やさない
    // (NoteCreateService.ts の `data.renote.userId !== user.id` ガード) ため、
    // 別 user から renote して両 backend で count が動く形にする (#2276)。
    const renoter = await signupUser(
      request,
      `pwrn${Date.now().toString().slice(-9)}`,
      DEFAULT_TEST_PASSWORD,
    );
    const renote = await callApi(request, 'notes/create', {
      i: renoter.token,
      renoteId: parentId,
      visibility: 'public',
    });
    expect(renote.status()).toBe(200);

    // /notes/:id を browser で navigate して親 note text が render される
    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/notes/${parentId}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);
    await page.waitForFunction(
      (text) => document.body.textContent?.includes(text) ?? false,
      parentText,
      { timeout: 20_000 },
    );

    // backend の renote chain 形成確認
    const showResp = await callApi(request, 'notes/show', { i: root.token, noteId: parentId });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    expect(shown.renoteCount, 'parent.renoteCount should be 1 after renote').toBe(1);
  });
});
