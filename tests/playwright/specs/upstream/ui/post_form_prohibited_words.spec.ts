/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 管理画面の禁止語 (meta の prohibitedWords) に当たる本文を投稿フォームから送ると、
// エラーが出て、ノートが作られないことを確かめる (#4043)。
//
// meta は全 spec で共有するので、始める前の値を覚えて finally で戻す
// (shard-plan.mjs の writesMeta により meta の project で流れる)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAs } from '../../../fixtures/ui_auth';
import { clickByTestId } from '../../../fixtures/ui_click';
import { openPostForm, setPostText, waitForApiBody, waitForSubmitEnabled } from '../../../fixtures/ui_post_form';

test.describe('UI: post form prohibited words', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(90_000);

  test('a note with a prohibited word shows an error and is not created', async ({ page, baseURL, request }) => {
    const before = await callApi(request, 'admin/meta', { i: root.token });
    expect(before.ok(), 'admin/meta を読めなかった').toBe(true);
    const original = ((await before.json()).prohibitedWords ?? []) as string[];
    const word = `pwbanned${Date.now()}`;

    try {
      const set = await callApi(request, 'admin/update-meta', { i: root.token, prohibitedWords: [...original, word] });
      expect(set.ok(), `admin/update-meta failed: ${set.status()}`).toBe(true);

      const user = await signupUser(request, randomUsername('pwban'));
      await uiSigninAs(page, request, baseURL!, user);
      await openPostForm(page);
      const text = `pw-prohibited ${word} here`;
      await setPostText(page, text);
      await waitForSubmitEnabled(page);

      const sent = waitForApiBody(page, 'notes/create');
      const resp = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/notes/create');
      await clickByTestId(page, 'post-form-submit');
      expect((await sent).text).toBe(text);
      const res = await resp;
      expect(res.status()).toBe(400);
      expect((await res.json()).error?.code).toBe('CONTAINS_PROHIBITED_WORDS');

      // エラーのダイアログが出る (os.alert の type: error)
      await expect(page.getByText('Cannot post because it contains prohibited words.')).toBeVisible({ timeout: 10_000 });

      const notes = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id })).json();
      expect(notes).toHaveLength(0);
    } finally {
      const restored = await callApi(request, 'admin/update-meta', { i: root.token, prohibitedWords: original });
      expect(restored.ok(), `prohibitedWords を戻せなかった: ${restored.status()}`).toBe(true);
    }
  });
});
