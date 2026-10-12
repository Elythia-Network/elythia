/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// note 詳細で renote button (ti-repeat) → renote menu → "Renote" item
// (ti-fw ti-repeat) → /api/notes/create が renoteId 付きで round-trip
// する write-flow spec。
//
// MkNote.vue の renote button click は getRenoteMenu (get-note-menu.ts:591)
// を popup する。menu の最初の通常 item が "Renote" (ti-fw ti-repeat、
// line 643)。click すると notes/create を { renoteId: ... } で叩く
// (line 666)。toast 表示で完了。
//
// confirm 無し直接 API。popup menu 系の最も短い flow の 1 つ。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon } from '../../../fixtures/ui_click';

test.describe('UI: note renote via menu flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('open renote menu → click Renote → /api/notes/create with renoteId', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 note を create
    const noteText = `pw-note-renote-${Date.now()}`;
    const createResp = await callApi(request, 'notes/create', {
      i: root.token,
      text: noteText,
      visibility: 'home',
    });
    expect(createResp.status()).toBe(200);
    const noteId = (await createResp.json()).createdNote.id;
    expect(noteId).toBeTruthy();

    // 2. note 詳細ページを開く
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/notes/${noteId}`, {
      waitUntil: 'domcontentloaded',
    });

    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      noteText,
      { timeout: 20_000 },
    );

    // 3. footer の renote button (= ti-repeat icon を持つ button) を click。
    // note footer には ti-arrow-back-up (reply) / ti-repeat (renote) /
    // ti-mood-plus (reaction) / ti-dots (more) などが並ぶ。renote 専用に
    // ti-repeat icon を探す (menu item 側は ti-fw ti-repeat なので重複しない)。
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some(
          (b) =>
            b.querySelector('i.ti-repeat') !== null &&
            !b.querySelector('i.ti-fw'),
        );
      },
      { timeout: 15_000 },
    );
    // MkNote.vue:139 の renote button は `@mousedown.prevent` のため click
    // event では popup が開かない。mousedown を dispatch する。詳細は
    // note_delete_via_menu.spec.ts のコメント参照。
    await page.evaluate(() => {
      const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
      const target = btns.find(
        (b) =>
          b.querySelector('i.ti-repeat') !== null &&
          !b.querySelector('i.ti-fw'),
      );
      target?.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0 }));
    });

    // 4. popup menu の "Renote" item (ti-fw ti-repeat) を click → notes/create
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some((b) => b.querySelector('i.ti-fw.ti-repeat') !== null);
      },
      { timeout: 10_000 },
    );

    const renoteResp = page.waitForResponse(
      async (r) => {
        if (!r.url().includes('/api/notes/create')) return false;
        if (r.status() >= 300) return false;
        // renote と quote 用の menu item 両方とも ti-repeat を出すが、quote
        // は post composer 経由で notes/create を叩くため、こちらは renote
        // path (= request body に renoteId が含まれ text が無い) を識別する。
        try {
          const body = await r.request().postDataJSON();
          return body && body.renoteId !== undefined;
        } catch {
          return false;
        }
      },
      { timeout: 15_000 },
    );
    await clickButtonWithIcon(page, 'i.ti-fw.ti-repeat');
    const renote = await renoteResp;
    const body = await renote.json();
    expect(body.createdNote.renoteId).toBe(noteId);
  });
});
