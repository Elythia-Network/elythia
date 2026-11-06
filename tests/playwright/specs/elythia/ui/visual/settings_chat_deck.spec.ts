/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 見た目の差分 (#4054): 設定 (部品をまとめて撮れる)、チャット、デッキ。
// 種まきと固定のしかたは fixtures/visual.ts。

import { test } from '@playwright/test';
import { isTsBackend } from '../../../../fixtures/backend';
import { pageBody, waitForMountedPage } from '../../../../fixtures/route_check';
import { coversRoutes, gotoRoute } from '../../../../fixtures/ui_route';
import { SCHEMES, snap, startAsAlice } from '../../../../fixtures/visual';

// デッキは端末の設定のプロファイルで決める。ホームのタイムライン・メンション・
// ウィジェットの 3 列。通知の列は Redis を空にされると中身が変わるので使わない
// (fixtures/visual.ts の visualWorld)
const DECK = {
  'deck.profile': 'Main',
  'deck.profiles': [{
    name: 'Main',
    id: 'vr-deck',
    columns: [
      { id: 'vr-tl', type: 'tl', tl: 'home', name: null, width: 360 },
      { id: 'vr-mentions', type: 'mentions', name: null, width: 330 },
      { id: 'vr-widgets', type: 'widgets', name: null, width: 300, widgets: [{ name: 'profile', id: 'vr-dw-profile', data: {} }, { name: 'memo', id: 'vr-dw-memo', data: {} }] },
    ],
    layout: [['vr-tl'], ['vr-mentions'], ['vr-widgets']],
  }],
};

test.describe('visual: settings, chat and deck', () => {
  test.skip(isTsBackend, '見た目の差分は Elythia の frontend だけを撮る');
  test.setTimeout(90_000);

  for (const scheme of SCHEMES) {
    test(`settings top and preferences (${scheme})`, coversRoutes('/settings', '/settings/preferences'), async ({ page, baseURL, request }) => {
      await startAsAlice(page, request, baseURL!, scheme, { viewport: { width: 1280, height: 1200 } });
      await gotoRoute(page, baseURL!, '/settings');
      await pageBody(page).getByText('Preferences').first().waitFor();
      await snap(page, `settings-${scheme}`);
      await gotoRoute(page, baseURL!, '/settings/preferences');
      await pageBody(page).getByText('configure the overall behavior').waitFor();
      await snap(page, `settings-preferences-${scheme}`);
    });

    test(`profile settings (${scheme})`, coversRoutes('/settings/profile'), async ({ page, baseURL, request }) => {
      await startAsAlice(page, request, baseURL!, scheme, { viewport: { width: 1280, height: 1200 } });
      await gotoRoute(page, baseURL!, '/settings/profile');
      await pageBody(page).locator('input').first().waitFor();
      await snap(page, `settings-profile-${scheme}`);
    });

    test(`chat with a user (${scheme})`, coversRoutes('/chat/user/:userId'), async ({ page, baseURL, request }) => {
      const w = await startAsAlice(page, request, baseURL!, scheme);
      await gotoRoute(page, baseURL!, `/chat/user/${w.bob.id}`);
      const body = pageBody(page);
      await body.getByText('Great, thanks.').waitFor();
      await body.getByText('Hi Bob, how are you?').waitFor();
      await snap(page, `chat-${scheme}`);
    });

    test(`deck with three columns (${scheme})`, async ({ page, baseURL, request }) => {
      await startAsAlice(page, request, baseURL!, scheme, { ui: 'deck', accountPrefs: DECK, viewport: { width: 1280, height: 900 } });
      await waitForMountedPage(page);
      await page.getByText('Four pictures.').first().waitFor();
      await page.getByText('a mention for you.').first().waitFor();
      await snap(page, `deck-${scheme}`);
    });
  }
});
