/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /clips/:id の MkPageHeader headerActions 内 ti-trash danger button →
// confirm dialog OK → /api/clips/delete round-trip する write-flow spec。
//
// clip.vue:181-200 の headerActions 配列に edit (ti-pencil) + delete
// (ti-trash danger) の 2 個。delete handler は os.confirm warning →
// 承諾後 clips/delete を叩く。clip_edit_save spec の sister として、
// 削除 path を strict 化する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { NOT_FOUND_STATUS } from '../../../fixtures/backend';
import { clickButtonWithIcon, clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: /clips/:id delete button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('header trash button → confirm OK → /api/clips/delete', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 clip を API で create
    const clipName = `pw-clip-del-${Date.now()}`;
    const createResp = await request.post(`${baseURL}/api/clips/create`, {
      ignoreHTTPSErrors: true,
      data: { i: root.token, name: clipName, isPublic: false },
    });
    expect(createResp.status()).toBe(200);
    const clip = await createResp.json();
    const clipId: string = clip.id;
    expect(clipId).toBeTruthy();

    // 2. clip 詳細ページを開く
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/clips/${clipId}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      clipName,
      { timeout: 20_000 },
    );

    // 3. headerActions の ti-trash button (= delete) hydrate を待つ。
    // edit は ti-pencil なので両方 ti-* で区別する。
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some((b) => b.querySelector('i.ti-trash') !== null);
      },
      { timeout: 15_000 },
    );

    // delete button click → confirm dialog 出現
    await clickButtonWithIcon(page, 'i.ti-trash');

    // 4. confirm dialog OK click → API 呼出
    await page.waitForFunction(
      () => document.querySelector('[data-testid="modal-dialog-ok"]') !== null,
      { timeout: 10_000 },
    );

    const deleteResp = page.waitForResponse(
      (r) => r.url().includes('/api/clips/delete') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    await deleteResp;

    // 5. API 経由で削除確認 — clips/show は 404 + NO_SUCH_CLIP を返す
    // (clips/show の UUID c3c5fe33-d62c-44d2-9ea5-d997703f5c20)。
    const showResp = await request.post(`${baseURL}/api/clips/show`, {
      ignoreHTTPSErrors: true,
      data: { i: root.token, clipId },
    });
    expect(showResp.status()).toBe(NOT_FOUND_STATUS);
    const showBody = await showResp.json();
    expect(showBody.error?.code).toBe('NO_SUCH_CLIP');
    expect(showBody.error?.id).toBe('c3c5fe33-d62c-44d2-9ea5-d997703f5c20');
  });
});
