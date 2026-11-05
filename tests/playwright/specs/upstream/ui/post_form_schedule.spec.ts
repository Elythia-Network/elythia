/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 投稿フォームで日時を指定して予約すると、指定した `scheduledAt` で予約の下書きが
// 作られ、再読み込みの後も予約の一覧に出ることを確かめる (#4043)。
//
// 予約は notes/drafts/create に `isActuallyScheduled: true` で送られる
// (MkPostForm の postAsScheduled)。予約できる数は role の policy
// (scheduledNoteLimit、既定 1) で決まるので、spec ごとに利用者を作る。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickByTestId } from '../../../fixtures/ui_click';
import {
  clickMenuItem,
  clickOnlyTestId,
  openPostForm,
  setPostText,
  waitForApiBody,
  waitForSubmitEnabled,
} from '../../../fixtures/ui_post_form';

test.describe('UI: post form scheduled note', () => {
  test.setTimeout(120_000);

  test('scheduling sends scheduledAt and the note shows in the scheduled list', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwsched'));
    await uiSigninAs(page, request, baseURL!, user);

    const text = `pw-sched-${Date.now()}`;
    await openPostForm(page);
    await setPostText(page, text);

    // 2 日後の 12:34 (ブラウザの時刻帯) を、datetime-local の形とミリ秒の両方で作る
    const { local, epoch } = await page.evaluate(() => {
      const d = new Date(Date.now() + 2 * 24 * 60 * 60 * 1000);
      d.setHours(12, 34, 0, 0);
      const p = (n: number) => String(n).padStart(2, '0');
      return {
        local: `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`,
        epoch: d.getTime(),
      };
    });

    await clickOnlyTestId(page, 'post-form-other-settings');
    await clickMenuItem(page, 'Schedule note...');
    const input = page.locator('input[type="datetime-local"]');
    await expect(input).toHaveCount(1, { timeout: 10_000 });
    await input.fill(local);
    await clickByTestId(page, 'modal-dialog-ok');

    await waitForSubmitEnabled(page);
    const sent = waitForApiBody(page, 'notes/drafts/create');
    const resp = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/notes/drafts/create');
    await clickOnlyTestId(page, 'post-form-submit');
    const body = await sent;
    expect(body.text).toBe(text);
    expect(body.isActuallyScheduled).toBe(true);
    expect(body.scheduledAt).toBe(epoch);
    expect((await resp).status()).toBe(200);

    const scheduled = await (await callApi(request, 'notes/drafts/list', { i: user.token, scheduled: true })).json();
    expect(scheduled).toHaveLength(1);
    expect(scheduled[0].text).toBe(text);
    expect(new Date(scheduled[0].scheduledAt).getTime()).toBe(epoch);
    // 予約しただけで、まだノートにはなっていない
    const notes = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id })).json();
    expect(notes).toHaveLength(0);

    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.locator('[data-testid="open-post-form"]').first()).toBeVisible({ timeout: 20_000 });
    await openPostForm(page);
    await clickOnlyTestId(page, 'post-form-account');
    await clickMenuItem(page, 'Scheduled notes list');
    const item = page.getByTestId('note-draft');
    await expect(item).toHaveCount(1, { timeout: 15_000 });
    await expect(item).toContainText(text);
  });
});
