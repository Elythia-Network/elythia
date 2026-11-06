/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// アカウントの設定 (#4056) のうち、メールの設定 (/settings/email)。
//
// メールの欄は、サーバーの enableEmail が切れていると MkDisableSection (inert) の中にあり、
// 操作できない。テストのスタックは切れているので、この spec の間だけ有効にして、終わったら
// 元に戻す。meta を書くので、meta の project で流れる。メールは送らない (SMTP を設定しない)。
// 効き目 (メールが届くか) は、backend がこの値を読んで送る処理を持たないので省く。

import { readFileSync } from 'node:fs';
import { expect, type Page, test } from '@playwright/test';
import { type AccountRow, labeledSwitch, runAccountRow } from '../../../../fixtures/settings_table';
import { coversRoutes } from '../../../../fixtures/ui_route';
import { apiOk } from '../../../../fixtures/ui_timeline';

async function expectSwitch(page: Page, label: string, on: boolean): Promise<void> {
  await expect(await labeledSwitch(page, label)).toHaveAttribute('data-checked', String(on));
}

const EMAIL_SKIP = 'メールで知らせる設定だが、backend にこの値を読んで送る処理が無い (internal/ で grep して、model / entity / i/update / admin/users / signup だけ)。本家も通知のメール送信はコメントアウトされている';

/** A row for one of the five email notification switches. */
function emailTypeRow(type: string, label: string, on: boolean): AccountRow {
  return {
    name: `emailNotificationTypes(${type}) → ${on}`, route: '/settings/email',
    set: async ({ page }) => (await labeledSwitch(page, label)).click(),
    read: (me) => (me.emailNotificationTypes as string[]).includes(type), expected: on,
    shows: ({ page }) => expectSwitch(page, label, on),
    effect: { skip: EMAIL_SKIP },
  };
}

test.describe('settings: account (email)', () => {
  test.setTimeout(60_000);
  let root: { token: string };
  let enableEmailBefore: boolean;
  test.beforeAll(async ({ request }) => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    enableEmailBefore = (await apiOk(request, 'admin/meta', { i: root.token })).enableEmail;
    await apiOk(request, 'admin/update-meta', { i: root.token, enableEmail: true });
  });
  test.afterAll(async ({ request }) => {
    await apiOk(request, 'admin/update-meta', { i: root.token, enableEmail: enableEmailBefore });
  });

  const ROWS: AccountRow[] = [
    {
      name: 'receiveAnnouncementEmail → false', route: '/settings/email',
      set: async ({ page }) => (await labeledSwitch(page, 'Receive notifications from this instance')).click(),
      read: (me) => me.receiveAnnouncementEmail, expected: false,
      shows: ({ page }) => expectSwitch(page, 'Receive notifications from this instance', false),
      effect: { skip: EMAIL_SKIP },
    },
    // 既定は follow と receiveFollowRequest だけが入っている (internal/entity/user.go)
    emailTypeRow('mention', 'Mentions', true),
    emailTypeRow('reply', 'Replies', true),
    emailTypeRow('quote', 'Quotes', true),
    emailTypeRow('follow', 'New followers', false),
    emailTypeRow('receiveFollowRequest', 'Received follow requests', false),
  ];
  for (const row of ROWS) test(row.name, coversRoutes(row.route), ({ page, request, baseURL }) => runAccountRow({ page, request, baseURL }, row));
});
