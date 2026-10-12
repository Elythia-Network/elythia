/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 残 content type の API 作成 → UI 詳細 page navigation。
//
// content_pages.spec.ts (channel / clip / gallery) に続く batch。本 spec は
// announcement / page / antenna / drive folder の 4 系統を覆う。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { deleteAntennasNamed } from '../../../fixtures/quota';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: extra content pages render after API-side create', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  // signin chain を含む test は 60s 余裕が必要
  test.setTimeout(60_000);

  test('navigate to /announcements after admin creates an announcement', async ({ page, baseURL, request }) => {
    const title = `playwright-announce ${Date.now()}`;
    const create = await callApi(request, 'admin/announcements/create', {
      i: root.token,
      title,
      text: 'playwright announcement body',
      imageUrl: null,
    });
    expect(create.status()).toBe(200);

    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/announcements`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // /announcements page は announcements/list を hit して title を render
    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      title,
      { timeout: 20_000 },
    );
  });

  // /@:username/pages/:pageName route は frontend (page.vue) が
  // pages/show に { username, name } を投げる。mk-go の ShowRequest は #955
  // で username パラメータも accept するよう拡張済 (= ShowByUsername で
  // host=NULL の local user を引いて user.id + name で page lookup)。
  test('navigate to /@alice/pages/:slug after creating a user page via API', async ({ page, baseURL, request }) => {
    const title = `playwright-page ${Date.now()}`;
    const slug = `pw${Date.now()}`;
    const create = await callApi(request, 'pages/create', {
      i: root.token,
      title,
      name: slug,
      summary: 'playwright test page',
      content: [{ type: 'text', text: 'page body content' }],
      variables: [],
      script: '',
      eyeCatchingImageId: null,
      font: 'sans-serif',
      alignCenter: false,
      hideTitleWhenPinned: false,
    });
    expect(create.status()).toBe(200);
    const createdPage = await create.json();
    expect(createdPage.id, 'pages/create should return id').toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/@${root.username}/pages/${slug}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      title,
      { timeout: 20_000 },
    );
  });

  // 作った antenna は片付ける。root を共有しているので放置すると
  // antennaLimit (既定 5) を使い切って無関係な spec が create で落ちる (#2264)。
  const createdAntennas: string[] = [];
  test.afterEach(async ({ request }) => {
    await deleteAntennasNamed(request, root.token, createdAntennas);
    createdAntennas.length = 0;
  });

  test('navigate to /my/antennas after creating an antenna via API', async ({ page, baseURL, request }) => {
    const name = `playwright-antenna ${Date.now()}`;
    createdAntennas.push(name);
    const create = await callApi(request, 'antennas/create', {
      i: root.token,
      name,
      src: 'all',
      keywords: [['playwright']],
      excludeKeywords: [[]],
      users: [],
      caseSensitive: false,
      withReplies: false,
      withFile: false,
      notify: false,
    });
    expect(create.status()).toBe(200);

    // /my/antennas は authenticated 前提 (= antennas/list を hit する)
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/antennas`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      name,
      { timeout: 20_000 },
    );
  });

  test('navigate to /my/drive after creating a drive folder via API', async ({ page, baseURL, request }) => {
    const folderName = `pw-folder-${Date.now()}`;
    const create = await callApi(request, 'drive/folders/create', {
      i: root.token,
      name: folderName,
    });
    expect(create.status()).toBe(200);

    // /my/drive は authenticated 前提 (= drive/folders を hit する)
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/drive`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      folderName,
      { timeout: 20_000 },
    );
  });
});
