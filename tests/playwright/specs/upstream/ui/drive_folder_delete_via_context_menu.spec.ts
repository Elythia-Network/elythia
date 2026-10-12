/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/drive で folder を右クリック → context menu (popupMenu) → "Delete"
// item (ti-fw ti-trash) → /api/drive/folders/delete 直接 round-trip する
// write-flow spec。
//
// MkDrive.folder.vue:280 onContextmenu は popupMenu で 5+ items を出す。
// Delete item は ti-trash + danger で、deleteFolder() は confirm 無しで
// drive/folders/delete を直接叩く (line 251)。空 folder のみ削除可。
//
// Playwright で右クリックを模擬するには contextmenu event を dispatch
// する。Playwright の page.click は left button のみ、page.locator.click
// で button: 'right' option もあるが、構造ベース selector 中心の本 PR
// では native event dispatch で揃える。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { NOT_FOUND_STATUS } from '../../../fixtures/backend';
import { clickButtonWithIcon } from '../../../fixtures/ui_click';

test.describe('UI: /my/drive folder delete via context menu flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  // #977 drift fix で endpoint 別の upstream-canonical UUID を返すように
  // 修正済み (drive/folders/show: `d74ab9eb-...`)。
  test('contextmenu folder → Delete → /api/drive/folders/delete', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 empty folder を create via API
    const folderName = `pw-folder-del-${Date.now()}`;
    const createResp = await callApi(request, 'drive/folders/create', {
      i: root.token,
      name: folderName,
    });
    expect(createResp.status()).toBe(200);
    const folderId = (await createResp.json()).id;
    expect(folderId).toBeTruthy();

    // 2. /my/drive を開く
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/my/drive`, {
      waitUntil: 'domcontentloaded',
    });

    // folder name が body に出るまで待つ
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      folderName,
      { timeout: 20_000 },
    );

    // 3. 該当 folder element を探して contextmenu event を dispatch。
    // MkDrive.folder.vue の root div は draggable="true" attribute を持つ
    // 唯一の (folder name text を含む) 要素。これで child count に依存
    // しない安定 selector になる。bubbling で listener (= 同 root に bind)
    // に到達する。
    await page.evaluate((n) => {
      const draggables = Array.from(
        document.querySelectorAll('[draggable="true"]'),
      ) as HTMLElement[];
      const target = draggables.find((el) => (el.textContent ?? '').includes(n));
      if (!target) return;
      target.dispatchEvent(
        new MouseEvent('contextmenu', {
          bubbles: true,
          cancelable: true,
          button: 2,
          buttons: 2,
          view: window,
        }),
      );
    }, folderName);

    // 4. popup menu の "Delete" item (ti-fw ti-trash) を待って click
    const deleteResp = page.waitForResponse(
      (r) => r.url().includes('/api/drive/folders/delete') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickButtonWithIcon(page, 'i.ti-fw.ti-trash');
    await deleteResp;

    // 5. API 経由で削除確認 — drive/folders/show は 404 + NO_SUCH_FOLDER を返す
    // (#977 drift fix 後の UUID `d74ab9eb-bb09-4bba-bf24-fb58f761e1e9`)。
    const showResp = await callApi(request, 'drive/folders/show', {
      i: root.token,
      folderId,
    });
    expect(showResp.status()).toBe(NOT_FOUND_STATUS);
    const showBody = await showResp.json();
    expect(showBody.error?.code).toBe('NO_SUCH_FOLDER');
    expect(showBody.error?.id).toBe('d74ab9eb-bb09-4bba-bf24-fb58f761e1e9');
  });
});
