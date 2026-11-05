/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 初期設定のウィザードを最後まで進めると、各段で入れた値 (名前・自己紹介・
// フォローの承認制・連合するか) が保存され、再読み込みしてもウィザードが出直さない
// ことを確かめる (#4049)。
//
// 連合するかを選ぶ段 (5 段目) は Elythia 独自 (登録した直後の利用者は federationStatus
// が pending で、ウィザードの最後に決める)。登録のダイアログと同じく、pending で
// 利用者を作る。
//
// ほかの spec は `uiSigninAs` でウィザードを抑止してからサインインする。ここでは
// ウィザードを出したいので、抑止せずにフォームでサインインする (`uiSigninViaForm`)。
//
// 値は、送った値 (i/update と i/update-federation) と、API で読み直した i で確かめる。

import { expect, type Page, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername } from '../../../fixtures/auth';
import { uiSigninViaForm } from '../../../fixtures/ui_auth';
import { clickSwitchByLabel, clickWhenReady } from '../../../fixtures/ui_click';
import { openFolderByLabel } from '../../../fixtures/ui_folder';
import { clickOnlyTestId, waitForApiBody } from '../../../fixtures/ui_post_form';

// 項目 (data-testid の付いた MkInput / MkTextarea) の中の Save を押す
function clickSaveIn(page: Page, testId: string): Promise<void> {
  return clickWhenReady(page, `${testId} の Save`, (id: string) =>
    Array.from(document.querySelectorAll(`[data-testid="${id}"] button`)).find((b) => (b.textContent ?? '').trim() === 'Save'), testId);
}

// 次の段へ進み、前の段が消えて、次の段の見出しが出るまで待つ。段は Transition (out-in)
// で入れ替わり、見出しは先に変わるので、見出しだけを待って続けて押すと、消えかけの
// 前の段のボタンを押して空振りする。押したボタンが DOM から外れるまで待つ
async function next(page: Page, testId: 'user-setup-continue' | 'user-setup-finish', header: string): Promise<void> {
  const button = await page.waitForFunction((id: string) => {
    const els = document.querySelectorAll(`[data-testid="${id}"]`);
    return els.length === 1 ? els[0] : null;
  }, testId, { timeout: 15_000 });
  await button.evaluate((el) => (el as HTMLElement).click());
  await page.waitForFunction((el) => !(el as Element).isConnected, button, { timeout: 15_000 });
  await button.dispose();
  // 見出しは窓の上の帯に出る。最初の段のボタン (「Profile settings」) と同じ文字なので、
  // ボタンの中のものは数えない
  await page.waitForFunction((h: string) => {
    const dialog = document.querySelector('[data-testid="user-setup-dialog"]');
    if (dialog === null) return false;
    return Array.from(dialog.querySelectorAll('span, div'))
      .some((el) => el.closest('button') === null && Array.from(el.childNodes).some((c) => c.nodeType === Node.TEXT_NODE && (c.textContent ?? '').trim() === h));
  }, header, { timeout: 15_000 });
}

test.describe('UI: account setup wizard', () => {
  test.setTimeout(120_000);

  test('each step saves what was entered, and the wizard does not come back after a reload', async ({ page, baseURL, request }) => {
    const username = randomUsername('pwwizard');
    const signup = await callApi(request, 'signup', { username, password: DEFAULT_TEST_PASSWORD, federationStatus: 'pending' });
    expect(signup.status(), `signup failed: ${await signup.text()}`).toBe(200);
    const user = await signup.json() as { id: string; token: string };

    await uiSigninViaForm(page, baseURL!, username);
    // 外枠 (user-setup-dialog) は大きさを持たないので、中のボタンで見る
    await expect(page.getByTestId('user-setup-dialog').getByTestId('user-setup-continue')).toBeVisible({ timeout: 20_000 });
    await next(page, 'user-setup-continue', 'Profile settings');

    // 1 段目: 名前と自己紹介 (どちらも Save を押して保存する)
    const name = `PW Wizard ${Date.now()}`;
    const description = `pw-wizard-bio-${Date.now()}`;
    const nameInput = page.locator('[data-testid="user-setup-user-name"] input');
    await expect(nameInput).toHaveCount(1, { timeout: 15_000 });
    await nameInput.fill(name);
    const nameSent = waitForApiBody(page, 'i/update');
    await clickSaveIn(page, 'user-setup-user-name');
    expect(await nameSent).toMatchObject({ name });
    // 名前の Save は待機のダイアログ (os.apiWithDialog) を出し、成功の後も約 1 秒残る。
    // その間は focus trap が後ろの要素を inert にするので、fill がフォーカスを当てられず
    // 値が入らない。ダイアログが閉じて inert が外れるまで待ってから入れる
    const descInput = page.locator('[data-testid="user-setup-user-description"] textarea');
    await expect.poll(() => descInput.evaluate((el) => el.closest('[inert]') === null), { timeout: 15_000 }).toBe(true);
    await descInput.fill(description);
    await expect(descInput).toHaveValue(description);
    const descSent = waitForApiBody(page, 'i/update');
    await clickSaveIn(page, 'user-setup-user-description');
    expect(await descSent).toMatchObject({ description });
    await next(page, 'user-setup-continue', 'Privacy settings');

    // 2 段目: フォローを承認制にする
    await openFolderByLabel(page, 'Follow requests require approval');
    const lockSent = waitForApiBody(page, 'i/update');
    await clickSwitchByLabel(page, 'Follow requests require approval');
    expect(await lockSent).toMatchObject({ isLocked: true });
    await next(page, 'user-setup-continue', 'Follow');

    // 3 段目 (おすすめのフォロー) と 4 段目 (プッシュ通知) はそのまま進む
    await next(page, 'user-setup-continue', 'Push notifications');
    await next(page, 'user-setup-continue', 'Federation settings');

    // 5 段目: 連合しないを選ぶ (既定は連合する)
    await clickSwitchByLabel(page, 'Federate with other servers');
    const fedSent = waitForApiBody(page, 'i/update-federation');
    await next(page, 'user-setup-finish', 'Done');
    expect((await fedSent).federationStatus).toBe('off');
    await clickOnlyTestId(page, 'user-setup-close');
    await expect(page.getByTestId('user-setup-close')).toHaveCount(0, { timeout: 10_000 });

    const me = await (await callApi(request, 'i', { i: user.token })).json();
    expect(me.name).toBe(name);
    expect(me.description).toBe(description);
    expect(me.isLocked).toBe(true);
    expect(me.federationStatus).toBe('off');

    // 終えたことはアカウントの設定 (registry) に残り、再読み込みしても出直さない
    await expect.poll(async () => (await (await callApi(request, 'i/registry/get', {
      i: user.token, scope: ['client', 'base'], key: 'accountSetupWizard',
    })).json()), { timeout: 15_000 }).toBe(-1);
    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('open-post-form').first()).toBeVisible({ timeout: 15_000 });
    await page.waitForLoadState('networkidle');
    await expect(page.getByTestId('user-setup-continue')).toHaveCount(0);
  });
});
