/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 投稿フォームで「連合なし」(ロケットのボタン) を切り替えて投稿すると、
// `localOnly: true` が送られることを確かめる (#4043)。
//
// 初回は確認のダイアログが出る (MkPostForm の toggleLocalOnly)。「Disable」を
// 選んだときだけ切り替わる。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickButtonByText } from '../../../fixtures/ui_click';
import { clickOnlyTestId, openPostForm, setPostText, submitPostForm } from '../../../fixtures/ui_post_form';

test.describe('UI: post form local only', () => {
  test.setTimeout(90_000);

  test('turning federation off sends localOnly=true', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwlocal'));
    await uiSigninAs(page, request, baseURL!, user);
    await openPostForm(page);
    await setPostText(page, `pw-local-only-${Date.now()}`);

    await clickOnlyTestId(page, 'post-form-local-only');
    await expect(page.getByText('Really disable federation?', { exact: true })).toBeVisible({ timeout: 10_000 });
    await clickButtonByText(page, 'Disable');
    // 切り替わるとアイコンが ti-rocket-off になる。送る前に状態が変わったことを待つ
    await expect(page.getByTestId('post-form-local-only').locator('i.ti-rocket-off')).toHaveCount(1);

    const sent = await submitPostForm(page);
    expect(sent.localOnly).toBe(true);

    const notes = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id })).json();
    expect(notes).toHaveLength(1);
    expect(notes[0].localOnly).toBe(true);
  });
});
