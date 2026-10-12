/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/profile の "advancedSettings" MkFolder を expand → isCat
// switch を toggle → /api/i/update が走る write-flow spec。
//
// profile.vue の isCat / isBot は collapsed な MkFolder の中にいるため、
// folder header を click して expand しないと switch が DOM に出ない。
// expand 後の最初の checkbox が isCat、その次が isBot。
//
// page には deep watcher (= profile object 全体) が居り、isCat の v-model
// 変化で `save()` が呼ばれて os.apiWithDialog('i/update', { isCat,... })
// が走る (profile.vue:207-211)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /settings/profile isCat toggle flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('expand advancedSettings folder → toggle isCat → /api/i/update', async ({
    page,
    baseURL,
    request,
  }) => {
    // 値 strict assertion を効かせるため、初期 state を false に reset。
    // prior run の累積で isCat=true のまま残っていると click で false に
    // 反転して期待と逆になるので、明示的に既知 state から始める。
    await callApi(request, 'i/update', { i: root.token, isCat: false });

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/profile`, {
      waitUntil: 'domcontentloaded',
    });

    // 最初の MkInput (= name) が hydrate するまで待つ
    await page.waitForFunction(
      () => document.querySelectorAll('input').length >= 2,
      { timeout: 20_000 },
    );

    // settings/profile の MkFolder は metadataEdit (1 つ目) と
    // advancedSettings (2 つ目) の 2 つ。advancedSettings の折りたたみ
    // 内には isCat / isBot の 2 個の MkSwitch (= input[type=checkbox])
    // があり、折りたたみ状態では DOM に出ない (Vue の v-if 相当)。
    // 「click 前 checkbox 数」を測ってから headers[1] (= advancedSettings)
    // を click し、checkbox 数が 2 増えたことで expand 成功を verify する。
    // metadataEdit (headers[0]) を expand しても checkbox は増えないので
    // 必ず headers[1] を選ぶこと (#969 review round の bug fix)。
    const beforeCheckboxes = await page.evaluate(
      () => document.querySelectorAll('input[type="checkbox"]').length,
    );

    await clickWhenReady(page, '「Advanced settings」の folder-header', () => {
      const headers = Array.from(
        document.querySelectorAll('[data-testid="folder-header"]'),
      ) as HTMLElement[];
      return headers[1];
    });
    await page.waitForFunction(
      (n) => document.querySelectorAll('input[type="checkbox"]').length >= n + 2,
      beforeCheckboxes,
      { timeout: 10_000 },
    );

    // folder 内最初の switch (= isCat) を click → i/update 走る
    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/update') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickWhenReady(page, 'isCat の checkbox', (before) => {
      const cbs = Array.from(
        document.querySelectorAll('input[type="checkbox"]'),
      ) as HTMLInputElement[];
      // expand 前に居なかった checkbox 群の先頭が isCat
      return cbs[before];
    }, beforeCheckboxes);
    const update = await updateResp;
    const body = await update.json();
    // beforeAll の API reset で isCat=false から始まるので、click 後は
    // 必ず true が返る strict assertion。i/update が MeDetailed shape を
    // 返すことも併せて verify (#969)。
    expect(body.id).toBeTruthy();
    expect(body.isCat).toBe(true);
  });
});
