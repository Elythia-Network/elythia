/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 通報と絵文字の申請が、審査する人の通知欄に出ることを確かめる (Elythia 独自、
// #2868 / #2987)。本家は通報を通知欄に出さず、絵文字の申請の機能も無い。
//
// 受け取る人は、モデレーターで絵文字を管理できるロールを付けた、この spec で
// 作った利用者にする。root は使わない: 通報の通知はモデレーターごとに 1 件へ
// まとめ、10 分の間は作り直さない (#3200) ので、ほかの spec の通報で root の
// 通知が既にあると、この spec の通報は root の通知欄の先頭に上がってこない。
//
// 通報の行には通報した人の名前と、その通報を開くボタン (`reportId` 付きの
// /admin/abuses) が出る。申請の行には絵文字の名前と、審査の画面へのボタンが出る。
// 通報の行は、英語の UI で日本語の文言が混ざらないことも見る (#4047)。
// 作ったロール・通報・申請は finally で片付ける。

import { expect, test } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { isTsBackend } from '../../../fixtures/backend';
import { uploadTinyPNG } from '../../../fixtures/files';
import { pollForNotification } from '../../../fixtures/notifications';
import { type RootFixture, uiSigninAs } from '../../../fixtures/ui_auth';
import { JAPANESE } from '../../../fixtures/ui_locale';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, freshUser, pageBody, runTag } from '../../../fixtures/ui_timeline';

test.describe('UI: abuse report and emoji request notifications', () => {
  test.skip(isTsBackend, '通報と絵文字の申請の通知は Elythia 独自 (#2868 / #2987)');
  test.setTimeout(90_000);

  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test('a reviewer sees the report and the emoji request with links to handle them', coversRoutes('/my/notifications'), async ({ page, baseURL, request }) => {
    const tag = runTag('mod');
    const reviewer = await freshUser(request, 'pwnmr');
    const reporter = await freshUser(request, 'pwnmu');
    const target = await freshUser(request, 'pwnmt');
    const role = await apiOk(request, 'admin/roles/create', {
      i: root.token,
      name: `${tag}-reviewer`,
      description: '',
      color: null,
      iconUrl: null,
      target: 'manual',
      condFormula: {},
      isPublic: false,
      isModerator: true,
      isAdministrator: false,
      isExplorable: false,
      asBadge: false,
      preserveAssignmentOnMoveAccount: false,
      canEditMembersByModerator: false,
      displayOrder: 0,
      // 絵文字の申請は、モデレーターではなく絵文字を管理できる人に届く (#2987)
      policies: { canManageCustomEmojis: { useDefault: false, priority: 0, value: true } },
    });
    let reportId: string | undefined;
    let applicationId: string | undefined;
    try {
      await apiOk(request, 'admin/roles/assign', { i: root.token, roleId: role.id, userId: reviewer.id });

      await apiOk(request, 'users/report-abuse', { i: reporter.token, userId: target.id, comment: `${tag}-report` });
      const reportNote = await pollForNotification(request, reviewer.token, (n) => n.type === 'abuseReport');
      reportId = (reportNote as unknown as { reportId: string }).reportId;
      expect(reportId).toBeTruthy();

      const file = await uploadTinyPNG(request, baseURL!, reporter.token, `${tag}.png`);
      const emojiName = `${tag}_emoji`;
      const application = await apiOk(request, 'emoji-application/create', { i: reporter.token, name: emojiName, license: 'CC0', fileId: file.id });
      applicationId = application.id;
      await pollForNotification(request, reviewer.token, (n) => n.type === 'emojiApplicationReceived');

      await uiSigninAs(page, request, baseURL!, reviewer);
      await gotoRoute(page, baseURL!, '/my/notifications');
      const body = pageBody(page);

      const report = body.locator('[data-testid="notification-abuseReport"]');
      await expect(report).toHaveCount(1, { timeout: 20_000 });
      await expect(report).toContainText(reporter.username);
      await expect(report.locator(`a[href="/admin/abuses?reportId=${reportId}"]`), '通報を開くボタンがこの通報を指していない').toHaveCount(1);
      // 通報の本文は通知に出さない (#2868)
      await expect(report).not.toContainText(`${tag}-report`);
      // 英語の UI で、通報の通知の文言が日本語のまま出ない (#4047)。en-US に無い語は ja-JP に戻る
      await expect(report).toContainText('submitted a report');
      await expect(report).toContainText('Review the report');
      await expect(report).not.toContainText(JAPANESE);

      const emoji = body.locator('[data-testid="notification-emojiApplicationReceived"]');
      await expect(emoji).toHaveCount(1);
      await expect(emoji).toContainText(`A request to register the emoji :${emojiName}: arrived`);
      await expect(emoji.locator('a[href="/custom-emojis-manager?tab=applications"]')).toHaveCount(1);
    } finally {
      if (applicationId !== undefined) await apiOk(request, 'emoji-application/cancel', { i: reporter.token, applicationId });
      if (reportId !== undefined) await apiOk(request, 'admin/resolve-abuse-user-report', { i: root.token, reportId });
      await apiOk(request, 'admin/roles/delete', { i: root.token, roleId: role.id });
    }
  });
});
