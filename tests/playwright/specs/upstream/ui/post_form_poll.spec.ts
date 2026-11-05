/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 投稿フォームで投票を作って投稿すると、選択肢・複数選択・期限が `poll` として
// 送られることを確かめる (#4043)。
//
// 期限は「End after...」で秒数を入れる。単位の既定は秒なので、7200 を入れると
// expiredAfter は 7200000 (ミリ秒) になる (MkPollEditor の get)。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickButtonByText, clickSwitchByLabel } from '../../../fixtures/ui_click';
import { fillInputByLabel } from '../../../fixtures/ui_input';
import { clickMenuItem, clickOnlyTestId, openPostForm, setPostText, submitPostForm } from '../../../fixtures/ui_post_form';

test.describe('UI: post form poll', () => {
  test.setTimeout(90_000);

  test('a poll with choices, multiple and expiry is sent as poll', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwpoll'));
    await uiSigninAs(page, request, baseURL!, user);
    await openPostForm(page);
    await setPostText(page, `pw-poll-${Date.now()}`);

    await clickOnlyTestId(page, 'post-form-poll');
    await clickButtonByText(page, 'Add');
    const choices = ['pw-choice-a', 'pw-choice-b', 'pw-choice-c'];
    for (const [i, choice] of choices.entries()) {
      const input = page.locator(`input[placeholder="Choice ${i + 1}"]`);
      await expect(input).toHaveCount(1);
      await input.fill(choice);
    }
    await clickSwitchByLabel(page, 'Allow selecting multiple choices');

    // 期限の MkSelect は mousedown でメニューを開く
    const opened = await page.evaluate(() => {
      const label = Array.from(document.querySelectorAll('div')).filter(
        (d) => d.children.length === 0 && (d.textContent ?? '').trim() === 'End poll',
      );
      if (label.length !== 1) return false;
      const select = label[0].parentElement?.querySelector('[tabindex="0"]');
      if (!select) return false;
      select.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0 }));
      return true;
    });
    expect(opened, '期限の選択肢を開けなかった').toBe(true);
    await clickMenuItem(page, 'End after...');
    await fillInputByLabel(page, 'Duration', '7200');

    const sent = await submitPostForm(page);
    expect(sent.poll).toEqual({
      choices,
      multiple: true,
      expiresAt: null,
      expiredAfter: 7_200_000,
    });

    const notes = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id })).json();
    expect(notes).toHaveLength(1);
    expect(notes[0].poll.choices.map((c: { text: string }) => c.text)).toEqual(choices);
    expect(notes[0].poll.multiple).toBe(true);
    expect(notes[0].poll.expiresAt).not.toBeNull();
  });
});
