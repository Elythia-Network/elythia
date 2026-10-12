/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// note 詳細ページの SPA navigation。
//
// API で note を作成 → /notes/:id を開いて MkNotePage が note 内容を render
// することを verify。post_note.spec.ts は composer 経由の create flow を
// テストするが、本 spec は read-side (= 既存 note を URL で開く) の hydration
// が壊れていないかを検出する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import type { RootFixture } from '../../../fixtures/ui_auth';

test.describe('UI: note detail page rendering', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(30_000);

  test('navigate to /notes/:id and the note text is rendered', async ({ page, baseURL, request }) => {
    // API で note を 1 つ作成 (= UI を経由しないことで signin scope の依存
    // を切る、本 spec は read-side のみ verify)
    const noteText = `playwright-detail-${Date.now()}`;
    const createResp = await callApi(request, 'notes/create', {
      i: root.token,
      text: noteText,
      visibility: 'public',
    });
    expect(createResp.status()).toBe(200);
    const created = await createResp.json();
    const noteId = created.createdNote.id;

    // SPA で /notes/:id を navigate。public visibility なので未認証でも開ける。
    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/notes/${noteId}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // body に noteText が visible になるまで待つ (= MkNotePage が hydration
    // 完了して note content を render した状態)。
    await page.waitForFunction(
      (text) => document.body.textContent?.includes(text) ?? false,
      noteText,
      { timeout: 20_000 },
    );
  });
});
