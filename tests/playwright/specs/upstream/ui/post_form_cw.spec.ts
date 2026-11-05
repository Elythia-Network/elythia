/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 投稿フォームで CW を付けて投稿すると、`cw` が送られ、一覧では本文が折りたたまれて
// 「Show content」で開くことを確かめる (#4043)。
//
// 一覧は投稿した利用者のプロフィールで見る。ノートが 1 つしか無いので、CW の
// ボタンも本文もちょうど 1 つに決まる。

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { callApi } from '../../../fixtures/api';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickOnlyTestId, openPostForm, setFieldValue, setPostText, submitPostForm } from '../../../fixtures/ui_post_form';
import { gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: post form CW', () => {
  test.setTimeout(90_000);

  test('posting with CW sends cw, and the note is folded until "Show content"', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwcw'));
    await uiSigninAs(page, request, baseURL!, user);

    const text = `pw-cw-body-${Date.now()}`;
    const cw = `pw-cw-${Date.now()}`;
    await openPostForm(page);
    await setPostText(page, text);
    await clickOnlyTestId(page, 'post-form-cw-toggle');
    await setFieldValue(page, 'post-form-cw', cw);
    const sent = await submitPostForm(page);
    expect(sent.cw).toBe(cw);
    expect(sent.text).toBe(text);

    // サーバーが保存した値でも確かめる (送った値と保存した値の両方が要る)
    const notes = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id })).json();
    expect(notes).toHaveLength(1);
    expect(notes[0].cw).toBe(cw);

    await gotoRoute(page, baseURL!, `/@${user.username}/notes`);
    await expect(page.getByText(cw, { exact: true })).toBeVisible({ timeout: 20_000 });
    const content = page.getByTestId('note-content');
    await expect(content).toHaveCount(1);
    // 折りたたまれている間は本文の要素が v-show で隠れる
    await expect(content).toBeHidden();
    await clickOnlyTestId(page, 'note-cw-toggle');
    await expect(content).toBeVisible();
    await expect(content).toContainText(text);
  });
});
