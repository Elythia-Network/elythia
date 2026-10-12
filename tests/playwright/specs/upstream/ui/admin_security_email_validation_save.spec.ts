/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/security の "Active Email Validation" MkFolder を expand →
// switch を toggle → form footer の Save → /api/admin/update-meta が
// round-trip する write-flow spec。
//
// admin/security.vue:69-? の emailValidationForm は MkFolder 内で複数の
// MkSwitch / MkInput を持つ。switch 変更で form.modified=true → footer
// MkFormFooter の Save が出現する pattern。admin_security_ip_logging_save
// の sister として、別 folder に対する form-save flow を担保する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText, clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /admin/security email validation form save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('expand Active Email Validation folder → toggle switch → Save → /api/admin/update-meta', async ({
    page,
    baseURL,
    request,
  }) => {
    // setup: 既知 state (false) に reset。
    await callApi(request, 'admin/update-meta', {
      i: root.token,
      enableActiveEmailValidation: false,
    });

    try {
      await uiSigninAsRoot(page, baseURL, root);
      await page.goto(`${baseURL}/admin/security`, {
        waitUntil: 'domcontentloaded',
      });

      await page.waitForFunction(
        () => document.querySelectorAll('[data-testid="folder-header"]').length >= 3,
        { timeout: 20_000 },
      );

      // "Active Email Validation" folder を expand
      await clickWhenReady(page, '「Active Email Validation」の folder-header', () => {
        const headers = Array.from(
          document.querySelectorAll('[data-testid="folder-header"]'),
        ) as HTMLElement[];
        const target = headers.find((h) =>
          (h.textContent ?? '').includes('Active Email Validation'),
        );
        return target;
      });

      // **checkbox を index で引かない。** この folder は Enable /
      // Verifymail / TrueMail の 3 つを持ち、他の folder が開いていれば
      // 先頭はそちらのものになる。押す対象が変わっても spec は緑のままに
      // なるので (#2620)、folder に scope して先頭 = Enable を引く。
      // MkFolder の root は role="group" で header と本文の両方を含む。
      await clickWhenReady(page, '「Active Email Validation」の Enable スイッチ', (l: string) => {
        const group = Array.from(document.querySelectorAll('[role="group"]')).find((g) =>
          (g.querySelector('[data-testid="folder-header"]')?.textContent ?? '').includes(l),
        );
        return group?.querySelector('input[type="checkbox"]');
      }, 'Active Email Validation');

      await page.waitForFunction(
        () => {
          const btns = Array.from(document.querySelectorAll('button'));
          return btns.some((b) => (b.textContent ?? '').includes('Save'));
        },
        { timeout: 10_000 },
      );

      const updateResp = page.waitForResponse(
        (r) => r.url().includes('/api/admin/update-meta') && r.status() < 300,
        { timeout: 15_000 },
      );
      await clickButtonContainingText(page, 'Save');
      await updateResp;

      // update-meta が返ってきたことだけを見ると、別の switch を押していても
      // 緑になる。実際に変わった field を meta で確かめる。
      const metaResp = await callApi(request, 'admin/meta', { i: root.token });
      expect(metaResp.status()).toBe(200);
      expect((await metaResp.json()).enableActiveEmailValidation).toBe(true);
    } finally {
      // cleanup: enable* / verifymail / truemail 系を全部 false に戻す。
      // on のまま残ると以降の signup 系 spec が email validation で fail
      // する isolation 破壊を引き起こす。同 form の他 field も spec 中で
      // toggle される可能性があるので一律 false 化。
      await callApi(request, 'admin/update-meta', {
        i: root.token,
        enableActiveEmailValidation: false,
        enableVerifymailApi: false,
        enableTruemailApi: false,
      });
    }
  });
});
