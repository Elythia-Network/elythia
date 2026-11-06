/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 見た目の差分 (#4054): ナビバーとウィジェットの欄、about のページ、管理画面、
// 横断の部品 (メニュー・確認のダイアログ・トースト)、モバイルの幅。
// 種まきと固定のしかたは fixtures/visual.ts。

import { test } from '@playwright/test';
import { isTsBackend } from '../../../../fixtures/backend';
import { pageBody } from '../../../../fixtures/route_check';
import { clickButtonByText } from '../../../../fixtures/ui_click';
import { clickMenuItem, waitForMenuItem } from '../../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../../fixtures/ui_route';
import { SCHEMES, snap, startAsAlice } from '../../../../fixtures/visual';

test.describe('visual: layout, about, admin and shared parts', () => {
  test.skip(isTsBackend, '見た目の差分は Elythia の frontend だけを撮る');
  test.setTimeout(90_000);

  for (const scheme of SCHEMES) {
    test(`navbar (full and icon only) and the widgets column (${scheme})`, coversRoutes('/'), async ({ page, baseURL, request }) => {
      // 1280 では右の欄が細いので、ウィジェットが並ぶ幅で撮る
      await startAsAlice(page, request, baseURL!, scheme, { widgets: ['profile', 'instanceInfo', 'memo'], viewport: { width: 1600, height: 900 } });
      await gotoRoute(page, baseURL!, '/');
      await pageBody(page).getByText('Four pictures.').waitFor();
      await snap(page, `navbar-widgets-${scheme}`);
      // ナビバーの下の「<」で、アイコンだけの表示に切り替える
      await page.locator('button:has(i.ti-chevron-left)').first().click();
      await page.locator('button:has(i.ti-chevron-right)').first().waitFor();
      await snap(page, `navbar-icon-only-${scheme}`);
    });

    test(`about pages (${scheme})`, coversRoutes('/about', '/about-elythia'), async ({ page, baseURL, request }) => {
      await startAsAlice(page, request, baseURL!, scheme, { viewport: { width: 1280, height: 1200 } });
      await gotoRoute(page, baseURL!, '/about');
      const body = pageBody(page);
      await body.getByText('Statistics').waitFor();
      // 利用者数とノート数は、他の spec が作った数で変わる。2 つの見出しを両方含む、いちばん内側の枠を隠す
      const stats = body.locator('div').filter({ has: page.getByText('Users', { exact: true }) }).filter({ has: page.getByText('Notes', { exact: true }) }).last();
      await snap(page, `about-${scheme}`, { mask: [stats] });
      await gotoRoute(page, baseURL!, '/about-elythia');
      await pageBody(page).getByText('Misskey').first().waitFor();
      await snap(page, `about-elythia-${scheme}`);
    });

    test(`admin top with the menu (${scheme})`, coversRoutes('/admin'), async ({ page, baseURL, request }) => {
      await startAsAlice(page, request, baseURL!, scheme, { viewport: { width: 1280, height: 1000 } });
      // 未解決の通報があると、メニューの上に警告が 1 つ増える。通報を作って解決しない spec
      // (note_report_abuse_via_menu など) が同じ shard の前に流れるかで変わるので、空を返す
      await page.route('**/api/admin/abuse-user-reports', (route) => route.fulfill({ json: [] }));
      await gotoRoute(page, baseURL!, '/admin');
      await page.getByText('Quick actions').waitFor();
      // ダッシュボードの本体は、数・グラフ・稼働時間・版で毎回変わる。メニューと警告を撮る
      await snap(page, `admin-${scheme}`, { mask: [pageBody(page)] });
    });

    test(`note menu, confirm dialog and toast (${scheme})`, coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
      const w = await startAsAlice(page, request, baseURL!, scheme);
      await page.context().grantPermissions(['clipboard-read', 'clipboard-write']);
      await gotoRoute(page, baseURL!, `/notes/${w.aliceNoteId}`);
      const body = pageBody(page);
      await body.getByText('Hello from Alice.').first().waitFor();

      await body.getByTestId('note-menu').first().click();
      await waitForMenuItem(page, 'Delete');
      await snap(page, `note-menu-${scheme}`);

      await clickMenuItem(page, 'Delete');
      await page.getByTestId('modal-dialog-ok').waitFor();
      await snap(page, `confirm-dialog-${scheme}`);
      await clickButtonByText(page, 'Cancel');
      await page.getByTestId('modal-dialog-ok').waitFor({ state: 'detached' });

      await body.getByTestId('note-menu').first().click();
      await clickMenuItem(page, 'Copy link');
      await page.getByText('Copied').first().waitFor();
      await snap(page, `toast-${scheme}`);
    });

    test(`mobile timeline and bottom navigation (${scheme})`, coversRoutes('/'), async ({ page, baseURL, request }) => {
      await startAsAlice(page, request, baseURL!, scheme, { viewport: { width: 375, height: 812 } });
      await gotoRoute(page, baseURL!, '/');
      await pageBody(page).getByText('Four pictures.').waitFor();
      await snap(page, `mobile-timeline-${scheme}`);
    });
  }
});
