/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 管理画面の、サーバーの状態を見る Elythia の画面を通す (#4072)。ドライブの使用量 (#3077) は
// 再計算のボタンで集計し直した値が出ること、DB の健全性とサーバーの指標は読めた表示が出ること、
// サーバーのプラグインは組み込みが無いときの表示が出ることを見る。
//
// ドライブの使用量は、1 回再計算して件数を控えてからファイルを上げ、もう 1 回再計算して件数が増え、
// 応答の値がそのまま画面に出ることを見る。開いたときの値は前に集計した控えで、それより後の他の spec の
// アップロードを含まないので、基準にはしない。Playwright は workers 1 で流すので、2 回の再計算の間に
// 他の spec のアップロードや削除は入らない (手元で --workers を上げると成り立たない)。

import { expect, test } from '@playwright/test';
import { expectApiOk, rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { distinctPNG, uploadPNG } from '../../../fixtures/ui_drive';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

test.describe('admin: server status', () => {
  test.skip(isTsBackend, 'これらの画面の API は Elythia だけにある');
  test.setTimeout(90_000);

  test('drive usage is recalculated on demand', coversRoutes('/admin/files'), async ({ page, request, baseURL }) => {
    const root = rootFixture();
    await signinAdmin(page, baseURL!, root);
    await gotoRoute(page, baseURL!, '/admin/files');
    await page.getByTestId('files-tab-usage').click();
    const local = page.getByTestId('drive-usage-local');
    await expect(local).toBeVisible();
    const recalc = async (): Promise<number> => {
      const recalculated = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/drive/usage' && r.request().postDataJSON()?.forceRecalc === true, { timeout: 15_000 });
      await page.getByTestId('drive-usage-refresh').click();
      const resp = await recalculated;
      expect(resp.ok(), 'admin/drive/usage (forceRecalc) が失敗した').toBe(true);
      const count = (await resp.json() as { local: { count: number } }).local.count;
      await expect(local, '再計算の値が画面に出ない').toHaveAttribute('data-count', String(count));
      return count;
    };
    const before = await recalc();
    // root のドライブを汚さないよう、使い捨ての利用者で上げる (集計はローカル全体)
    const uploader = await signupUser(request, randomUsername('pwusage'));
    await uploadPNG(request, baseURL!, uploader.token, 'pw-usage.png', distinctPNG());
    expect(await recalc(), '上げたファイルが集計に入らない').toBe(before + 1);
  });

  test('database health shows a report', coversRoutes('/admin/database'), async ({ page, baseURL }) => {
    await signinAdmin(page, baseURL!, rootFixture());
    await gotoRoute(page, baseURL!, '/admin/database');
    const loaded = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/database-health', { timeout: 15_000 });
    await page.getByTestId('database-tab-health').click();
    expect((await loaded).ok(), 'admin/database-health が失敗した').toBe(true);
    await expect(page.getByTestId('database-health-report')).toBeVisible();
  });

  test('the overview shows the server metrics', coversRoutes('/admin/overview'), async ({ page, baseURL }) => {
    await signinAdmin(page, baseURL!, rootFixture());
    await gotoRoute(page, baseURL!, '/admin/overview');
    const metrics = page.getByTestId('server-metrics');
    await expect(metrics).toBeVisible({ timeout: 15_000 });
    expect(Number(await metrics.getAttribute('data-goroutines')), 'goroutine の数が出ない').toBeGreaterThan(0);
  });

  test('server plugins: none built in', coversRoutes('/admin/server-plugins'), async ({ page, request, baseURL }) => {
    const root = rootFixture();
    // このスタックはプラグインを組み込まずにビルドする
    const { plugins } = await expectApiOk(request, 'admin/server-plugins', { i: root.token }) as { plugins: unknown[] };
    expect(plugins, 'プラグインが組み込まれている (前提)').toEqual([]);
    await signinAdmin(page, baseURL!, root);
    await gotoRoute(page, baseURL!, '/admin/server-plugins');
    await expect(page.getByTestId('server-plugins-empty')).toBeVisible();
  });
});
