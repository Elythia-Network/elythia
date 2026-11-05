/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 投稿フォームをキーボードだけで扱えることを確かめる (#4043)。
//
// - `n` でフォームが開く (グローバルのホットキー)
// - Ctrl+Enter で、打った本文が notes/create に送られ、フォームが閉じる
// - Esc で、送らずに閉じる
//
// 本文もキーボードで打つ。native setter で入れると、キーの扱い (onKeydown) を
// 通らない経路になる。

import { expect, test, type Page } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { postTextValue, waitForApiBody, waitForPostForm } from '../../../fixtures/ui_post_form';

async function openWithHotkey(page: Page): Promise<void> {
  // 入力欄にフォーカスがあるとホットキーは効かないので外す
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await page.keyboard.press('n');
  await waitForPostForm(page);
  // 開いた直後は textarea にフォーカスが入る (autofocus)
  await expect(page.getByTestId('post-form-text')).toBeFocused();
}

test.describe('UI: post form keyboard', () => {
  test.setTimeout(90_000);

  test('n opens, Ctrl+Enter posts, Esc closes', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwkey'));
    await uiSigninAs(page, request, baseURL!, user);
    const form = page.getByTestId('post-form-text');

    await openWithHotkey(page);
    const text = `pw-key-${Date.now()}`;
    await page.keyboard.type(text);
    expect(await postTextValue(page)).toBe(text);
    const sent = waitForApiBody(page, 'notes/create');
    await page.keyboard.press('Control+Enter');
    expect((await sent).text).toBe(text);
    await expect(form).toHaveCount(0, { timeout: 10_000 });

    const notes = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id })).json();
    expect(notes).toHaveLength(1);
    expect(notes[0].text).toBe(text);

    // Esc は送らずに閉じる
    let posted = false;
    page.on('request', (r) => {
      if (new URL(r.url()).pathname === '/api/notes/create') posted = true;
    });
    await openWithHotkey(page);
    await page.keyboard.type('pw-key-discard');
    await page.keyboard.press('Escape');
    await expect(form).toHaveCount(0, { timeout: 10_000 });
    expect(posted).toBe(false);
    const after = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id })).json();
    expect(after).toHaveLength(1);
  });
});
