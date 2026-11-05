/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 投稿フォームで公開範囲を選んで投稿すると、選んだ `visibility` が送られることを
// 確かめる (#4043)。specified (Direct) は宛先を選ぶと `visibleUserIds` も送られる。
//
// 既定の公開範囲 (public) のまま送っても public が送られるので、public は
// いったん別の範囲を選んでから選び直す。選ぶ操作が効かない回帰を見逃さないため。

import { expect, test, type Page } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';
import { fillInputByLabel } from '../../../fixtures/ui_input';
import { clickOnlyTestId, openPostForm, setPostText, submitPostForm } from '../../../fixtures/ui_post_form';

// フォームの公開範囲のボタンは、選んだ範囲のアイコンを出す (MkPostForm.vue)
const VISIBILITY_ICON: Record<string, string> = {
  public: 'ti-world',
  home: 'ti-home',
  followers: 'ti-lock',
  specified: 'ti-mail',
};

async function chooseVisibility(page: Page, v: string): Promise<void> {
  await clickOnlyTestId(page, 'post-form-visibility');
  await clickOnlyTestId(page, `visibility-${v}`);
  // 選んだことがフォームに反映されるのを待つ。待たないと、間に挟んだ範囲が選べて
  // いなくても (ピッカーが効かなくても)、既定の public のテストは通ってしまう
  await expect(page.getByTestId('post-form-visibility').locator(`i.${VISIBILITY_ICON[v]}`)).toHaveCount(1);
}

test.describe('UI: post form visibility', () => {
  test.setTimeout(90_000);

  for (const visibility of ['public', 'home', 'followers'] as const) {
    test(`choosing ${visibility} sends visibility=${visibility}`, async ({ page, baseURL, request }) => {
      const user = await signupUser(request, randomUsername('pwvis'));
      await uiSigninAs(page, request, baseURL!, user);
      await openPostForm(page);
      const text = `pw-vis-${visibility}-${Date.now()}`;
      await setPostText(page, text);
      // 既定値のままでは「選んだ」ことにならないので、先に別の範囲へ動かす
      await chooseVisibility(page, visibility === 'home' ? 'followers' : 'home');
      await chooseVisibility(page, visibility);
      const sent = await submitPostForm(page);
      expect(sent.visibility).toBe(visibility);

      const notes = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id })).json();
      expect(notes).toHaveLength(1);
      expect(notes[0].visibility).toBe(visibility);
    });
  }

  test('choosing specified with a recipient sends visibleUserIds', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwvis'));
    const recipient = await signupUser(request, randomUsername('pwvisto'));
    await uiSigninAs(page, request, baseURL!, user);
    await openPostForm(page);
    const text = `pw-vis-specified-${Date.now()}`;
    await setPostText(page, text);
    await chooseVisibility(page, 'specified');

    // 宛先の欄の「+」(ti-plus) から利用者を選ぶ
    await clickWhenReady(page, '宛先を足すボタン', () =>
      Array.from(document.querySelectorAll('button._buttonPrimary')).find((b) => b.querySelector('i.ti-plus') !== null),
    );
    await fillInputByLabel(page, 'Username', recipient.username);
    await clickWhenReady(
      page,
      `検索結果の @${recipient.username}`,
      (name: string) =>
        Array.from(document.querySelectorAll('div._button')).find(
          (el) => (el.textContent ?? '').includes(`@${name}`) && el.querySelector('img') !== null,
        ),
      recipient.username,
    );
    await clickOnlyTestId(page, 'modal-window-ok');

    const sent = await submitPostForm(page);
    expect(sent.visibility).toBe('specified');
    expect(sent.visibleUserIds).toEqual([recipient.id]);

    // 保存された宛先を、宛先の利用者から読み直して確かめる
    const noteId = (await (await callApi(request, 'users/notes', { i: user.token, userId: user.id })).json())[0].id;
    const asRecipient = await callApi(request, 'notes/show', { i: recipient.token, noteId });
    expect(asRecipient.status()).toBe(200);
    expect((await asRecipient.json()).visibleUserIds).toEqual([recipient.id]);
  });
});
