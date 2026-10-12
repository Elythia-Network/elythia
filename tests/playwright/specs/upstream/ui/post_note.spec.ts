/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// UI 操作で signin → composer modal で note 投稿 → API 経由で note 取得確認の e2e。
//
// MkPostFormDialog は os.post() → popup(MkPostFormDialog) で **modal stack** に
// mount される。click → modal 出現 → textarea visible には CSS transition 分の
// 遅延があるため、textarea を探す前に MkModal の root が attach されるまで明示
// 待機する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: post note via composer modal', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(90_000);

  test('signin → open composer → fill text → submit → API confirms note', async ({ page, baseURL, request }) => {
    await uiSigninAsRoot(page, baseURL, root);

    // 投稿 form を開く。click 自体は CSS transition / overlay 干渉を avoid する
    // ため force: true。modal が popup で attach されるまで waitForFunction で
    // DOM を polling する (textarea が直接 visible になるまで Vue mount + CSS
    // transition で 1-2s かかる)。
    // programmatic click で navbar の hover state / animation 干渉を回避する
    // (force: true でも actionable check が race することがあるため、
    // dispatchEvent で直接 button の click handler を呼ぶ)。
    await clickByTestId(page, 'open-post-form');
    await page.waitForFunction(
      () => document.querySelector('[data-testid="post-form-text"]') !== null,
      { timeout: 15_000 },
    );

    // textarea に focus + 入力。modal 内の textarea は autofocus 属性付きだが、
    // Playwright の fill は focus ↔ value set を自前で行うので race にならない。
    const noteText = `playwright-ui-post ${Date.now()}`;
    // textarea は modal 内 + autofocus 属性付き。Playwright の locator click
    // / fill は modal の actionable check で stuck しやすいので、native focus
    // + value set + dispatchEvent('input') で Vue v-model に届ける。
    await page.evaluate((text) => {
      const t = document.querySelector('[data-testid="post-form-text"]') as HTMLTextAreaElement | null;
      if (!t) return;
      t.focus();
      // native HTMLTextAreaElement の value setter を経由して Vue が反応する
      // input event を dispatch する。
      const setter = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value')?.set;
      setter?.call(t, text);
      t.dispatchEvent(new Event('input', { bubbles: true }));
    }, noteText);
    // Vue の reactivity 反映 + canPost 計算待ち
    await page.waitForFunction(
      () => {
        const submit = document.querySelector('[data-testid="post-form-submit"]') as HTMLButtonElement | null;
        return submit !== null && !submit.disabled;
      },
      { timeout: 5_000 },
    );

    // submit ボタン (= [data-testid="post-form-submit"]) も同じく programmatic
    // click で動作させる (= modal 内に複数 submit ボタンがある可能性 + Vue
    // の click handler が disabled state 連動で再 binding されるため、
    // dispatchEvent ベースで確実に発火する)。
    const createResp = page.waitForResponse(
      (resp) => resp.url().includes('/api/notes/create') && resp.status() === 200,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'post-form-submit');
    const noteCreate = await createResp;
    const noteCreateBody = await noteCreate.json();
    expect(noteCreateBody.createdNote, 'POST /api/notes/create should return createdNote').toBeTruthy();
    expect(noteCreateBody.createdNote.text).toBe(noteText);

    // API 経由で note が取れる (= UI submit が backend まで届いた)
    const noteId = noteCreateBody.createdNote.id;
    const showResp = await callApi(request, 'notes/show', { i: root.token, noteId });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    expect(shown.id).toBe(noteId);
    expect(shown.text).toBe(noteText);
  });
});
