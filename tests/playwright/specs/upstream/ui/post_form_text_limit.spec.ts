/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 投稿フォームの本文が文字数の上限 (meta の maxNoteTextLength) を超えると送信ボタンが
// 押せず、上限ちょうどなら送れることを確かめる (#4043)。
//
// 上限は meta から読む。値を spec に書くと、上限の既定が変わったときに境界を
// 外れたまま緑になる。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { openPostForm, setPostText, submitPostForm } from '../../../fixtures/ui_post_form';

test.describe('UI: post form text length limit', () => {
  test.setTimeout(90_000);

  test('the submit button is disabled past maxNoteTextLength', async ({ page, baseURL, request }) => {
    const meta = await (await callApi(request, 'meta', {})).json();
    const max = meta.maxNoteTextLength as number;
    expect(max).toBeGreaterThan(0);

    const user = await signupUser(request, randomUsername('pwlimit'));
    await uiSigninAs(page, request, baseURL!, user);
    await openPostForm(page);

    const submit = page.getByTestId('post-form-submit');
    await setPostText(page, 'a'.repeat(max + 1));
    await expect(submit).toBeDisabled();
    // 残りの文字数が負で出る
    await expect(page.getByText('-1', { exact: true })).toBeVisible();

    await setPostText(page, 'a'.repeat(max));
    await expect(submit).toBeEnabled();
    const sent = await submitPostForm(page);
    expect((sent.text as string).length).toBe(max);

    const notes = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id })).json();
    expect(notes).toHaveLength(1);
    expect(notes[0].text.length).toBe(max);
  });
});
