/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/user?userId=:id で moderationNote MkTextarea (manualSave) を編集
// → Save click → /api/admin/update-user-note が round-trip する write-flow
// spec。
//
// admin-user.vue:54 で MkTextarea v-model="moderationNote" manualSave。
// 値変更で watch (line 307) が admin/update-user-note を即時呼ぶ。
// manualSave は MkButton "Save" を表示し、click で v-model を commit する。
// 本 spec は signup 直後 user (= moderationNote 空) を target にして
// note を埋めて Save → API persist を verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /admin/user moderationNote save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(90_000);

  test('edit moderationNote → save → /api/admin/update-user-note', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. target user を signup (root を mod-note しないように別 user)
    const username = `pwmn${Date.now().toString().slice(-9)}`;
    const target = await signupUser(request, username);
    expect(target.id).toBeTruthy();

    // 2. /admin/user/:userId を root として開く。upstream route は
    // path-based (router.definition.ts:381 = `/admin/user/:userId`)。
    // query string では not-found fallback に流れて hydrate しない。
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(
      `${baseURL}/admin/user/${target.id}`,
      { waitUntil: 'domcontentloaded' },
    );
    expect(resp!.status()).toBe(200);

    // user info hydrate を待つ
    await page.waitForFunction(
      (u) => document.body.textContent?.includes(u) ?? false,
      target.username,
      { timeout: 20_000 },
    );

    // 3. moderationNote textarea hydrate を待つ
    await page.waitForFunction(
      () => document.querySelectorAll('textarea').length >= 1,
      { timeout: 15_000 },
    );

    // textarea に note を投入。admin-user.vue で MkTextarea は moderationNote
    // の唯一なので最初の textarea で OK。
    const noteText = `pw-mod-note-${Date.now()}`;
    await page.evaluate((text) => {
      const tas = Array.from(document.querySelectorAll('textarea')) as HTMLTextAreaElement[];
      const target = tas[0];
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLTextAreaElement.prototype,
        'value',
      )?.set;
      setter?.call(target, text);
      target.dispatchEvent(new Event('input', { bubbles: true }));
      target.dispatchEvent(new Event('change', { bubbles: true }));
    }, noteText);

    // 4. manualSave button "Save" 出現を待ってから click → API 呼出
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button'));
        return btns.some((b) => (b.textContent ?? '').includes('Save'));
      },
      { timeout: 10_000 },
    );

    const updateResp = page.waitForResponse(
      (r) =>
        r.url().includes('/api/admin/update-user-note') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    await updateResp;

    // 5. API 経由で moderationNote 反映 verify
    const showResp = await callApi(request, 'admin/show-user', {
      i: root.token,
      userId: target.id,
    });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    // upstream の admin/show-user は id を返さない (show-user.ts の return は
    // profile 由来の field と roles/policies/signins のみ)。mk-go は追加で id を
    // 返すが、両 backend で通る assert にするため id は見ない (#2276)。
    expect(shown.moderationNote).toBe(noteText);
  });
});
