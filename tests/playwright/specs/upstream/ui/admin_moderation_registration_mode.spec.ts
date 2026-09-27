/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/moderation の「登録の受け付け方」(4 択) を選んだときに、admin/meta が
// その受け付け方になることを verify する write-flow spec (#3186)。
//
// 4 択にする前は「アカウント作成を許可」と「登録を承認制にする」の 2 つのスイッチで、
// 承認制を外すときに登録を閉じるかを聞くダイアログ (#2803) があった。受け付け方を
// 直接選ぶ形になり、ダイアログは要らなくなった。
//
// **どの列がどう動いたかまで見る。** update-meta が返ってきたことしか見ないと、
// どれを選んでも緑になる (#2620 と同じ壊れ方)。

import { readFileSync } from 'node:fs';
import { expect, type Page, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { isTsBackend } from '../../../fixtures/backend';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonByText } from '../../../fixtures/ui_click';

// mk-go 独自の文言は ja-JP にしか無く、en-US の frontend でも ja-JP が補う。
const LABELS = {
  open: '誰でも登録できる',
  invite: '招待制',
  approval: '承認制',
  closed: '受け付けない',
} as const;

test.describe('UI: /admin/moderation registration mode', () => {
  // 4 択と registrationClosed は mk-go 独自で、TS baseline (公式 frontend) には無い。
  test.skip(isTsBackend, 'registration mode は mk-go 独自');
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  const setMeta = async (request: Parameters<typeof callApi>[0], fields: Record<string, boolean>) => {
    const resp = await callApi(request, 'admin/update-meta', { i: root.token, ...fields });
    expect(resp.status()).toBeLessThan(300);
  };

  const readMeta = async (request: Parameters<typeof callApi>[0]) => {
    const resp = await callApi(request, 'admin/meta', { i: root.token });
    expect(resp.status()).toBe(200);
    return resp.json();
  };

  // **必ず開けて戻す。** 閉じたまま / 承認制のまま残ると、以降の signup spec が
  // 全滅する (#2620 と同じ壊れ方)。try/finally はテスト timeout で走らないので
  // afterEach に置く。
  test.afterEach(async ({ request }) => {
    await setMeta(request, { registrationClosed: false, approvalRequiredForSignup: false, disableRegistration: false });
  });

  const openModeration = async (page: Page, baseURL: string | undefined) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/moderation`, { waitUntil: 'domcontentloaded' });
    await expect(page.getByText(LABELS.closed, { exact: true })).toBeVisible({ timeout: 20_000 });
  };

  const choose = async (page: Page, label: string, confirm: boolean) => {
    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/admin/update-meta') && r.status() < 300,
      { timeout: 15_000 },
    );
    await page.getByText(label, { exact: true }).click();
    // 確認を出さずに送っていたら、ここで待っている応答が先に来て OK の click が
    // 「見つからない」で落ちる。
    if (confirm) await clickButtonByText(page, 'OK');
    await updateResp;
  };

  for (const c of [
    { from: { registrationClosed: false, approvalRequiredForSignup: false, disableRegistration: false }, to: 'invite', confirm: false,
      want: { registrationClosed: false, approvalRequiredForSignup: false, disableRegistration: true } },
    { from: { registrationClosed: false, approvalRequiredForSignup: false, disableRegistration: true }, to: 'approval', confirm: false,
      want: { registrationClosed: false, approvalRequiredForSignup: true, disableRegistration: false } },
    { from: { registrationClosed: false, approvalRequiredForSignup: false, disableRegistration: true }, to: 'open', confirm: true,
      want: { registrationClosed: false, approvalRequiredForSignup: false, disableRegistration: false } },
    // 閉じても承認制は残す (申請者の照会を開けておく / 解除後に戻れる)。
    { from: { registrationClosed: false, approvalRequiredForSignup: true, disableRegistration: false }, to: 'closed', confirm: true,
      want: { registrationClosed: true, approvalRequiredForSignup: true, disableRegistration: true } },
    // 閉じた状態から受け付け方を選ぶと、その受け付け方で再開する。
    { from: { registrationClosed: true, approvalRequiredForSignup: false, disableRegistration: true }, to: 'open', confirm: true,
      want: { registrationClosed: false, approvalRequiredForSignup: false, disableRegistration: false } },
  ] as const) {
    test(`${Object.entries(c.from).filter(([, v]) => v).map(([k]) => k).join('+') || 'open'} → ${c.to}`, async ({ page, baseURL, request }) => {
      await setMeta(request, c.from);
      await openModeration(page, baseURL);
      await choose(page, LABELS[c.to], c.confirm);

      const meta = await readMeta(request);
      expect({
        registrationClosed: meta.registrationClosed,
        approvalRequiredForSignup: meta.approvalRequiredForSignup,
        disableRegistration: meta.disableRegistration,
      }).toEqual(c.want);
    });
  }

  test('受け付けないの確認でキャンセルすると何も送らない', async ({ page, baseURL, request }) => {
    await setMeta(request, { registrationClosed: false, approvalRequiredForSignup: false, disableRegistration: true });
    await openModeration(page, baseURL);

    const sent: string[] = [];
    page.on('request', (r) => {
      if (r.url().includes('/api/admin/update-meta')) sent.push(r.url());
    });
    await page.getByText(LABELS.closed, { exact: true }).click();
    await clickButtonByText(page, 'Cancel');
    // ダイアログが閉じるのを待つ。閉じる前に meta を読むと、送っていてもまだ
    // 届いていないだけの状態を「送っていない」と読み違える。
    await page.waitForFunction(
      () => !Array.from(document.querySelectorAll('button')).some((b) => (b.textContent ?? '').trim() === 'Cancel'),
      undefined,
      { timeout: 15_000 },
    );

    expect(sent).toEqual([]);
    const meta = await readMeta(request);
    expect(meta.registrationClosed).toBe(false);
    expect(meta.disableRegistration).toBe(true);
  });
});
