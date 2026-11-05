/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 投稿フォームの下書き (サーバーに保存するもの) を、保存・一覧・復元・削除できる
// ことを確かめる (#4043)。
//
// - 保存: 「...」の「Save to Draft」で notes/drafts/create に本文と CW が送られる
// - 一覧と復元: 再読み込みの後、アカウントのメニューの「List of Drafts」に出て、
//   「Restore」で本文と CW がフォームに戻る
// - 削除: 一覧のゴミ箱で notes/drafts/delete が送られ、再読み込みの後も一覧に出ない
//
// フォームは打った内容を手元 (localStorage) にも自動で残すので、再読み込みの後に
// 開いたフォームには本文が入っていることがある。復元を確かめる前に空にしておく。

import { expect, test, type Page } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickByTestId } from '../../../fixtures/ui_click';
import {
  clickMenuItem,
  clickOnlyTestId,
  openPostForm,
  postTextValue,
  setFieldValue,
  setPostText,
  waitForApiBody,
} from '../../../fixtures/ui_post_form';

async function openDraftList(page: Page): Promise<void> {
  await clickOnlyTestId(page, 'post-form-account');
  await clickMenuItem(page, 'List of Drafts');
}

async function reloadSignedIn(page: Page): Promise<void> {
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.locator('[data-testid="open-post-form"]').first()).toBeVisible({ timeout: 20_000 });
}

test.describe('UI: post form drafts', () => {
  test.setTimeout(120_000);

  test('save, list, restore and delete a draft', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwdraft'));
    await uiSigninAs(page, request, baseURL!, user);

    const text = `pw-draft-${Date.now()}`;
    const cw = `pw-draft-cw-${Date.now()}`;
    await openPostForm(page);
    await setPostText(page, text);
    await clickOnlyTestId(page, 'post-form-cw-toggle');
    await setFieldValue(page, 'post-form-cw', cw);

    // 保存
    const created = waitForApiBody(page, 'notes/drafts/create');
    const createdResp = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/notes/drafts/create');
    await clickOnlyTestId(page, 'post-form-other-settings');
    await clickMenuItem(page, 'Save to Draft');
    const sent = await created;
    expect(sent.text).toBe(text);
    expect(sent.cw).toBe(cw);
    expect((await createdResp).status()).toBe(200);

    const saved = await (await callApi(request, 'notes/drafts/list', { i: user.token, scheduled: false })).json();
    expect(saved).toHaveLength(1);
    expect(saved[0].text).toBe(text);
    expect(saved[0].cw).toBe(cw);

    // 一覧と復元 (再読み込みの後)
    await reloadSignedIn(page);
    await openPostForm(page);
    await setPostText(page, '');
    await setFieldValue(page, 'post-form-cw', '');
    expect(await postTextValue(page)).toBe('');
    await openDraftList(page);
    const item = page.getByTestId('note-draft');
    await expect(item).toHaveCount(1, { timeout: 15_000 });
    // CW のある下書きは、一覧に本文ではなく CW を出す (getNoteSummary)
    await expect(item).toContainText(cw);
    await clickOnlyTestId(page, 'note-draft-restore');
    await expect.poll(() => postTextValue(page)).toBe(text);
    await expect(page.getByTestId('post-form-cw')).toHaveValue(cw);

    // 削除
    await openDraftList(page);
    await expect(page.getByTestId('note-draft')).toHaveCount(1, { timeout: 15_000 });
    const deleted = waitForApiBody(page, 'notes/drafts/delete');
    await clickOnlyTestId(page, 'note-draft-delete');
    await clickByTestId(page, 'modal-dialog-ok');
    expect((await deleted).draftId).toBe(saved[0].id);
    await expect
      .poll(async () => (await (await callApi(request, 'notes/drafts/list', { i: user.token, scheduled: false })).json()).length)
      .toBe(0);

    await reloadSignedIn(page);
    await openPostForm(page);
    await openDraftList(page);
    await expect(page.getByText('No drafts', { exact: true })).toBeVisible({ timeout: 15_000 });
    await expect(page.getByTestId('note-draft')).toHaveCount(0);
  });
});
