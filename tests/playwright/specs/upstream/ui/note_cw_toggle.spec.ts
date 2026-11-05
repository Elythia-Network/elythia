/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 一覧 (MkNote) の CW 付きのノートを、「Show content」で開き「Hide」で閉じられる
// ことを確かめる (#4043)。
//
// 一覧は投稿した利用者のノートのタブで見る。ノートが 1 つしか無いので、本文の要素も
// CW のボタンもちょうど 1 つに決まる。

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { createNote } from '../../../fixtures/notes';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickOnlyTestId } from '../../../fixtures/ui_post_form';
import { gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: note CW toggle', () => {
  test.setTimeout(90_000);

  test('Show content opens the body and Hide folds it again', async ({ page, baseURL, request }) => {
    const author = await signupUser(request, randomUsername('pwcwto'));
    const user = await signupUser(request, randomUsername('pwcwview'));
    const text = `pw-cw-body-${Date.now()}`;
    const cw = `pw-cw-${Date.now()}`;
    await createNote(request, author.token, { text, cw });
    await uiSigninAs(page, request, baseURL!, user);

    await gotoRoute(page, baseURL!, `/@${author.username}/notes`);
    await expect(page.getByText(cw, { exact: true })).toBeVisible({ timeout: 20_000 });
    const content = page.getByTestId('note-content');
    const toggle = page.getByTestId('note-cw-toggle');
    await expect(content).toHaveCount(1);
    await expect(content).toBeHidden();

    await clickOnlyTestId(page, 'note-cw-toggle');
    await expect(content).toBeVisible();
    await expect(content).toContainText(text);
    await expect(toggle).toContainText('Hide');

    await clickOnlyTestId(page, 'note-cw-toggle');
    await expect(content).toBeHidden();
    await expect(toggle).toContainText('Show content');
  });
});
