/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/email の emailNotification_mention switch を click → watch で
// /api/i/update が emailNotificationTypes 配列で round-trip する write-flow
// spec。
//
// email.vue:37 の MkSwitch v-model="emailNotification_mention" は watch
// (line 114) 経由で saveNotificationSettings → i/update を呼ぶ。
//
// **index で switch を引かない。** page 内には receiveAnnouncementEmail /
// mention / reply / quote / follow / receiveFollowRequest の 6 個が並ぶ。
// 旧実装は index 1 を押していたが、順番が変われば別の設定を切り替える。
// しかも「配列が返ってきた」しか見ていなかったので、どれを押しても緑に
// なっていた (#2620 の admin/moderation と同じ壊れ方)。
//
// 「最初の switch だけ動作する偽陽性」を排除する意図は先頭以外を選ぶことで
// 引き続き満たしている。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickSwitchByLabel } from '../../../fixtures/ui_click';

// i18n.ts._notification._types.mention の en-US 値。
const MENTION_LABEL = 'Mentions';

// upstream の既定値。ここから始めれば click は必ず「追加」方向に動く。
const BASELINE_TYPES = ['follow', 'receiveFollowRequest'];

test.describe('UI: /settings/email emailNotification_mention toggle flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('toggle emailNotification_mention switch → /api/i/update', async ({
    page,
    baseURL,
    request,
  }) => {
    // setup: 既知 state に reset。前 run が mention を立てたままだと click が
    // 「解除」方向になり、assert が状態依存になる。
    await callApi(request, 'i/update', {
      i: root.token,
      emailNotificationTypes: BASELINE_TYPES,
    });

    try {
      await uiSigninAsRoot(page, baseURL, root);
      await page.goto(`${baseURL}/settings/email`, {
        waitUntil: 'domcontentloaded',
      });

      const updateResp = page.waitForResponse(
        (r) => r.url().includes('/api/i/update') && r.status() < 300,
        { timeout: 15_000 },
      );
      await clickSwitchByLabel(page, MENTION_LABEL);
      const update = await updateResp;
      const body = await update.json();

      // **配列が返ってきたことで満足しない。** どの switch を押しても配列は
      // 返る。mention が増え、他の項目が消えていないことまで見る。
      expect(body.id).toBeTruthy();
      expect(body.emailNotificationTypes).toEqual(
        expect.arrayContaining([...BASELINE_TYPES, 'mention']),
      );
      expect(body.emailNotificationTypes).toHaveLength(BASELINE_TYPES.length + 1);
    } finally {
      await callApi(request, 'i/update', {
        i: root.token,
        emailNotificationTypes: BASELINE_TYPES,
      });
    }
  });
});
