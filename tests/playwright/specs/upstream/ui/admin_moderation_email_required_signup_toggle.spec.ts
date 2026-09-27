/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/moderation の emailRequiredForSignup switch を click → admin/meta に
// 反映されることを verify する write-flow spec。
//
// admin/moderation.vue:91 の MkSwitch v-model="emailRequiredForSignup" は
// @change で onChange_emailRequiredForSignup → admin/update-meta を直接
// 呼ぶ。confirm dialog なし、最も simple な toggle pattern。
//
// **index で switch を引かない。** 旧実装は `cbs[1]` を押していたが、mk-go
// 独自の「登録を承認制にする」switch (#2570) が enableRegistration の直後に
// 入ったため、2 番目は approvalRequiredForSignup になっていた。押している
// ものが違っても spec は緑で、approvalRequiredForSignup が on のまま残って
// **以降の全 spec の signup が 403 になっていた** (#2620)。登録の受け付け方は
// #3186 で 4 択のラジオになったが、ラベルで引く理由は変わらない。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { isTsBackend } from '../../../fixtures/backend';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickSwitchByLabel } from '../../../fixtures/ui_click';

// i18n.ts.emailRequiredForSignup の en-US 値。playwright.config.ts で
// locale を en-US に固定しているのでこの表記で引ける。
const EMAIL_REQUIRED_LABEL = 'Require email address for sign-up';

test.describe('UI: /admin/moderation emailRequiredForSignup toggle flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('toggle emailRequiredForSignup switch → /api/admin/update-meta', async ({
    page,
    baseURL,
    request,
  }) => {
    // setup: 両方を既知 state (false) に reset する。approvalRequiredForSignup
    // も揃えるのは、押し間違いを下の assert で検出できるようにするため。
    //
    // **disableRegistration も明示する (#2803)。** 承認制を ON→OFF にする更新で
    // 明示が無いと、サーバー側が「ゲートが 1 つも無い全開」を避けて
    // disableRegistration を true に倒す。押し間違いで承認制が on のまま来た
    // ときにここが登録を閉じてしまい、以降の signup spec が全滅する。
    await callApi(request, 'admin/update-meta', {
      i: root.token,
      emailRequiredForSignup: false,
      approvalRequiredForSignup: false,
      disableRegistration: false,
    });

    try {
      await uiSigninAsRoot(page, baseURL, root);
      await page.goto(`${baseURL}/admin/moderation`, {
        waitUntil: 'domcontentloaded',
      });

      const updateResp = page.waitForResponse(
        (r) => r.url().includes('/api/admin/update-meta') && r.status() < 300,
        { timeout: 15_000 },
      );
      await clickSwitchByLabel(page, EMAIL_REQUIRED_LABEL);
      await updateResp;

      // **どの field が変わったかまで見る。** update-meta が返ってきたことしか
      // 見ないと、別の switch を押しても緑になる (#2620)。
      const metaResp = await callApi(request, 'admin/meta', { i: root.token });
      expect(metaResp.status()).toBe(200);
      const meta = await metaResp.json();
      expect(meta.emailRequiredForSignup).toBe(true);
      // approvalRequiredForSignup は mk-go 独自の field で、TS には無い。
      if (!isTsBackend) {
        expect(meta.approvalRequiredForSignup).toBe(false);
      }
    } finally {
      // cleanup: 必ず false に戻す。emailRequiredForSignup が残ると以降の
      // signup spec が INVALID_PARAM (emailAddress required)、
      // approvalRequiredForSignup が残ると APPROVAL_REQUIRED (403)、
      // disableRegistration が残ると登録が閉じたままで全滅する。
      // pass / fail どちらでも cleanup を実行する。
      await callApi(request, 'admin/update-meta', {
        i: root.token,
        emailRequiredForSignup: false,
        approvalRequiredForSignup: false,
        disableRegistration: false,
      });
    }
  });
});
