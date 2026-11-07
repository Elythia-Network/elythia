/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /invite で招待コードを作る (#4072)。invite.vue は招待制 (disableRegistration) のときだけ作成の
// 画面を出すので、登録の受け付け方を招待制にしてから開き、終わったら控えた値に戻す。この spec は
// admin/update-meta を書くので meta の project で流れる (招待制の間に他の spec の signup が
// 断られないように)。
//
// 作ったコードは、知らせのダイアログと一覧に出て、invite/list で照会できることを見る。

import { expect, test } from '@playwright/test';
import { cleanupApi, expectApiOk, rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { callApi } from '../../../fixtures/api';
import { pageBody } from '../../../fixtures/route_check';
import { clickButtonContainingText } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

const KEYS = ['registrationClosed', 'approvalRequiredForSignup', 'disableRegistration'] as const;

test.describe('UI: /invite creates an invite code', () => {
  test.setTimeout(60_000);

  let original: Record<string, unknown> | undefined;
  test.beforeEach(async ({ request }) => {
    original = undefined;
    const root = rootFixture();
    const meta = await expectApiOk(request, 'admin/meta', { i: root.token });
    original = Object.fromEntries(KEYS.map((k) => [k, meta[k]]));
    await expectApiOk(request, 'admin/update-meta', { i: root.token, registrationClosed: false, approvalRequiredForSignup: false, disableRegistration: true });
  });
  test.afterEach(async ({ request }) => {
    if (original === undefined) return;
    const restored = await callApi(request, 'admin/update-meta', { i: rootFixture().token, ...original });
    expect.soft(restored.ok(), '登録の受け付け方を元に戻せなかった').toBe(true);
  });

  test('generate an invite code', coversRoutes('/invite'), async ({ page, request, baseURL }) => {
    const root = rootFixture();
    let code: string | undefined;
    let id: string | undefined;
    try {
      await signinAdmin(page, baseURL!, root);
      await gotoRoute(page, baseURL!, '/invite');
      const created = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/invite/create', { timeout: 15_000 });
      await clickButtonContainingText(page, 'Generate invite');
      const resp = await created;
      expect(resp.ok(), 'invite/create が失敗した').toBe(true);
      ({ code, id } = await resp.json() as { code: string; id: string });

      // 作ると、知らせのダイアログ (ページの外) を出すのと同時に一覧 (ページの中) の先頭に足す。
      // ダイアログはページの外にもコードがあることで見て、一覧はダイアログが消えてから見る
      const all = page.getByText(code);
      const inPage = pageBody(page).getByText(code);
      await expect.poll(async () => await all.count() - await inPage.count(), { message: 'ダイアログにコードが出ない' }).toBeGreaterThan(0);
      await page.getByTestId('modal-dialog-ok').click();
      await expect(page.getByTestId('modal-dialog-ok')).toHaveCount(0);
      await expect(inPage.first(), '一覧にコードが出ない').toBeVisible();
      const list = await expectApiOk(request, 'invite/list', { i: root.token, limit: 100 }) as { code: string }[];
      expect(list.map((x) => x.code), '一覧の API にコードが無い').toContain(code);
    } finally {
      if (id !== undefined) await cleanupApi(request, 'invite/delete', { i: root.token, inviteId: id });
    }
  });
});
