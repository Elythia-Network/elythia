/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 端末の設定 (#4056) のうち、連合を有効にしたサーバーでだけ部品が出るもの。
// instanceTicker の欄は、meta の federation が none のときは描かれない
// (preferences.vue の v-if)。テストのスタックは none なので、この spec の間だけ
// all にして、終わったら元に戻す。meta を書くので、meta の project で流れる。

import { readFileSync } from 'node:fs';
import { expect, type Locator, type Page, test } from '@playwright/test';
import { pageBody } from '../../../../fixtures/route_check';
import { coversPrefs, type PrefRow, prefTitle, runPrefRow } from '../../../../fixtures/settings_table';
import { gotoRoute } from '../../../../fixtures/ui_route';
import { apiOk, postNote, runTag } from '../../../../fixtures/ui_timeline';

const ROUTE = '/settings/preferences';

/** Opens the note page and returns the detailed note once it is ready. */
async function openNote(page: Page, baseURL: string, noteId: string): Promise<Locator> {
  await gotoRoute(page, baseURL, `/notes/${noteId}`);
  const note = pageBody(page).getByTestId('note-detailed');
  await expect(note.getByTestId('note-menu')).toBeVisible({ timeout: 20_000 });
  return note;
}

test.describe('settings: preferences (federated server)', () => {
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

  const ROWS: PrefRow[] = [
    {
      key: 'instanceTicker', route: ROUTE, control: { kind: 'select', label: 'Always show' }, value: 'always',
      effect: async ({ page, request, baseURL, user }) => {
        const note = await openNote(page, baseURL, await postNote(request, user, runTag('pwtick')));
        // ローカルのノートにも、このサーバーの名前のティッカーが出る。名前は frontend と同じ決め方で読む (@@/js/config.ts)
        const name = await page.evaluate(() => {
          const site = document.querySelector<HTMLMetaElement>('meta[property="og:site_name"]')?.content;
          const address = new URL(document.querySelector<HTMLMetaElement>('meta[property="instance_url"]')?.content || window.location.href);
          return (site === 'Elythia' || site === 'Misskey' || site == null) ? address.host : site;
        });
        await expect(note.getByText(name, { exact: true })).toBeVisible();
      },
    },
  ];
  for (const row of ROWS) test(prefTitle(row), coversPrefs(row.key), ({ page, request, baseURL }) => runPrefRow({ page, request, baseURL }, row));
});
