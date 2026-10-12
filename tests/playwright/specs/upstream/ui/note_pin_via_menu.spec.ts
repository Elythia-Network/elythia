/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 自分の note 詳細で 3-dot menu → "Pin" item (ti-fw ti-pin) を click →
// /api/i/pin が直接 round-trip する write-flow spec。
//
// get-note-menu.ts:250 togglePin は os.apiWithDialog('i/pin', { noteId })
// を直接呼ぶ (confirm 無し)。pin 上限 (default 5) で 422 が返ることが
// あるが、本 spec は signupUser 経由で fresh user の root で test する
// わけではなく root の既存 pinned 状況に依存する。先に pin 一覧を空に
// するため、setup で /api/i (root.pinnedNoteIds) を読んで全 unpin する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon } from '../../../fixtures/ui_click';

test.describe('UI: note 3-dot menu pin flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('open menu → click Pin → /api/i/pin', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. setup: root の pin 上限に当たらないよう既存 pin を全外し。
    const meResp = await callApi(request, 'i', { i: root.token });
    expect(meResp.status()).toBe(200);
    const me = await meResp.json();
    const pinned: string[] = me.pinnedNoteIds ?? [];
    for (const pinnedId of pinned) {
      await callApi(request, 'i/unpin', { i: root.token, noteId: pinnedId });
    }

    // 2. test 用 note を create
    const noteText = `pw-note-pin-${Date.now()}`;
    const createResp = await callApi(request, 'notes/create', {
      i: root.token,
      text: noteText,
      visibility: 'home',
    });
    expect(createResp.status()).toBe(200);
    const noteId = (await createResp.json()).createdNote.id;
    expect(noteId).toBeTruthy();

    // 3. note 詳細ページを開く
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/notes/${noteId}`, {
      waitUntil: 'domcontentloaded',
    });

    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      noteText,
      { timeout: 20_000 },
    );

    // 4. 3-dot menu (ti-dots) → Pin item (ti-fw ti-pin)
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some((b) => b.querySelector('i.ti-dots') !== null);
      },
      { timeout: 15_000 },
    );
    // MkNote.vue:157 は `@mousedown.prevent="showMenu()"` のため click event
    // では popup が開かない。mousedown を dispatch する。詳細は
    // note_delete_via_menu.spec.ts のコメント参照。
    await page.evaluate(() => {
      const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
      const target = btns.find((b) => b.querySelector('i.ti-dots') !== null);
      target?.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0 }));
    });

    const pinResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/pin') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickButtonWithIcon(page, 'i.ti-fw.ti-pin');
    await pinResp;

    // 5. API 経由で pinnedNoteIds に noteId が含まれること verify
    const me2Resp = await callApi(request, 'i', { i: root.token });
    expect(me2Resp.status()).toBe(200);
    const me2 = await me2Resp.json();
    expect((me2.pinnedNoteIds ?? []).includes(noteId)).toBe(true);

    // 6. cleanup: pinned note を unpin して spec 後始末
    await callApi(request, 'i/unpin', { i: root.token, noteId });
  });
});
