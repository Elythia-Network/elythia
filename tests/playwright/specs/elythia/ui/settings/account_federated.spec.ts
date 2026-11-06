/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// アカウントの設定 (#4056) のうち、連合を有効にしたサーバーでだけ部品が出るもの。
// インスタンスのミュートの折りたたみは、meta の federation が none のときは描かれない
// (mute-block.vue の v-if)。テストのスタックは none なので、この spec の間だけ all にして、
// 終わったら元に戻す。meta を書くので、meta の project で流れる。

import { readFileSync } from 'node:fs';
import { expect, type Page, test } from '@playwright/test';
import { pageBody } from '../../../../fixtures/route_check';
import { type AccountRow, runAccountRow } from '../../../../fixtures/settings_table';
import { clickFolderButton, fillFolderTextarea } from '../../../../fixtures/ui_folder';
import { coversRoutes } from '../../../../fixtures/ui_route';
import { apiOk } from '../../../../fixtures/ui_timeline';

const MUTED_HOST = 'remote.example';

/** The textarea in the folder whose header is `label`. */
function folderTextarea(page: Page, label: string) {
  return pageBody(page).locator('[role="group"]').filter({ has: page.getByTestId('folder-header').filter({ has: page.getByText(label, { exact: true }) }) }).last().locator('textarea');
}

test.describe('settings: account (federated server)', () => {
  test.setTimeout(60_000);
  let root: { token: string };
  let federationBefore: string;
  test.beforeAll(async ({ request }) => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    federationBefore = (await apiOk(request, 'admin/meta', { i: root.token })).federation;
    await apiOk(request, 'admin/update-meta', { i: root.token, federation: 'all' });
  });
  test.afterAll(async ({ request }) => {
    await apiOk(request, 'admin/update-meta', { i: root.token, federation: federationBefore });
  });

  const ROWS: AccountRow[] = [
    {
      name: `mutedInstances → [${MUTED_HOST}]`, route: '/settings/mute-block',
      set: async ({ page }) => {
        await fillFolderTextarea(page, 'Instance Mutes', MUTED_HOST);
        await clickFolderButton(page, 'Instance Mutes', 'Save');
      },
      read: (me) => me.mutedInstances, expected: [MUTED_HOST],
      shows: ({ page }) => expect(folderTextarea(page, 'Instance Mutes')).toHaveValue(MUTED_HOST),
      effect: { skip: 'そのホストのリモートの利用者のノートをタイムラインから除く (internal/core/notesfilter/muteblock.go)。この spec のスタックにはリモートのノートが無い' },
    },
  ];
  for (const row of ROWS) test(row.name, coversRoutes(row.route), ({ page, request, baseURL }) => runAccountRow({ page, request, baseURL }, row));
});
