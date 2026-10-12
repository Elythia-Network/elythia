/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// note 詳細で 3-dot menu → "Mute thread" item (ti-fw ti-message-off) →
// /api/notes/thread-muting/create 直接 round-trip する write-flow spec。
//
// get-note-menu.ts:421-426 の thread mute item は ti-message-off icon。
// click すると toggleThreadMute(true) → notes/thread-muting/create を
// 直接叩く (line 240)。confirm 無し。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon } from '../../../fixtures/ui_click';

test.describe('UI: note 3-dot menu mute thread flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('open menu → click Mute thread → /api/notes/thread-muting/create', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 note を create
    const noteText = `pw-note-tm-${Date.now()}`;
    const createResp = await callApi(request, 'notes/create', {
      i: root.token,
      text: noteText,
      visibility: 'home',
    });
    expect(createResp.status()).toBe(200);
    const noteId = (await createResp.json()).createdNote.id;
    expect(noteId).toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/notes/${noteId}`, {
      waitUntil: 'domcontentloaded',
    });

    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      noteText,
      { timeout: 20_000 },
    );

    // 3-dot menu (ti-dots) → Mute thread item (ti-fw ti-message-off)
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some((b) => b.querySelector('i.ti-dots') !== null);
      },
      { timeout: 15_000 },
    );
    // MkNote.vue:157 は `@mousedown.prevent="showMenu()"`。click event で
    // 反応しないので mousedown dispatch。詳細 note_delete_via_menu コメント。
    await page.evaluate(() => {
      const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
      const target = btns.find((b) => b.querySelector('i.ti-dots') !== null);
      target?.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0 }));
    });

    const muteResp = page.waitForResponse(
      (r) =>
        r.url().includes('/api/notes/thread-muting/create') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickButtonWithIcon(page, 'i.ti-fw.ti-message-off');
    await muteResp;

    // cleanup: API 経由で thread-mute を解除
    await callApi(request, 'notes/thread-muting/delete', {
      i: root.token,
      noteId,
    });
  });
});
