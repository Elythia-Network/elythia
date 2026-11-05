/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ノートのリアクションのボタンからピッカーで絵文字を選ぶと notes/reactions/create が
// 送られ、詳細のリアクションのタブに付けた人が出て、付けたリアクションをもう一度
// 押すと外れることを確かめる (#4043)。
//
// 付けたこと・外したことは、送った値と、API で読み直した myReaction の両方で見る。
// 付けた状態は、再読み込みの後 (リアクションのタブを直接開く) も残ることを見る。

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { createNote, showNoteRaw } from '../../../fixtures/notes';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickByTestId, clickWhenReady } from '../../../fixtures/ui_click';
import { clickOnlyTestId, waitForApiBody } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

const REACTION = '\u{1F44D}';

test.describe('UI: note reaction toggle', () => {
  test.setTimeout(90_000);

  test('react via the picker, see who reacted, and undo by pressing it again', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
    const author = await signupUser(request, randomUsername('pwreactto'));
    const user = await signupUser(request, randomUsername('pwreact'));
    const note = await createNote(request, author.token, { text: `pw-react-${Date.now()}` });
    await uiSigninAs(page, request, baseURL!, user);

    await gotoRoute(page, baseURL!, `/notes/${note.id}`);
    await expect(page.getByText(note.text!, { exact: true })).toBeVisible({ timeout: 20_000 });

    // 付ける (ピッカーの固定の絵文字から選ぶ)
    await clickOnlyTestId(page, 'note-react');
    const created = waitForApiBody(page, 'notes/reactions/create');
    await clickWhenReady(page, `ピッカーの ${REACTION}`, (e: string) => document.querySelector(`[data-emoji="${e}"]`), REACTION);
    expect(await created).toMatchObject({ noteId: note.id, reaction: REACTION });
    await expect
      .poll(async () => (await (await showNoteRaw(request, user.token, note.id)).json()).myReaction)
      .toBe(REACTION);

    // 再読み込みして、リアクションのタブに付けた人が出る
    await gotoRoute(page, baseURL!, `/notes/${note.id}/reactions`);
    await clickOnlyTestId(page, 'note-reaction-tab');
    // サインイン中の自分の名前はナビやウィジェットにも出るので、タブの中に絞る
    // (タブのボタンの親の親が、リアクションのタブの中身)
    const tab = page.getByTestId('note-reaction-tab').locator('xpath=../..');
    await expect(tab.getByText(`@${user.username}`, { exact: true })).toBeVisible({ timeout: 15_000 });

    // もう一度押すと外れる (確認のダイアログが出る)
    const deleted = waitForApiBody(page, 'notes/reactions/delete');
    await clickOnlyTestId(page, 'note-reaction');
    await clickByTestId(page, 'modal-dialog-ok');
    expect((await deleted).noteId).toBe(note.id);
    await expect
      .poll(async () => (await (await showNoteRaw(request, user.token, note.id)).json()).myReaction ?? null)
      .toBeNull();

    await gotoRoute(page, baseURL!, `/notes/${note.id}`);
    await expect(page.getByText(note.text!, { exact: true })).toBeVisible({ timeout: 20_000 });
    await expect(page.getByTestId('note-reaction')).toHaveCount(0);
  });
});
