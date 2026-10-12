/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// content (channel / clip / gallery) を API で作成 → UI で詳細 page を navigate
// して content title / name が render されることを verify する mixed e2e。
//
// API endpoint と SPA route の対応:
//   channels/create → /channels/:id
//   clips/create    → /clips/:id
//   gallery/posts/create → /gallery/post/:id
//
// 各 page は MkChannelPage / MkClipPage / MkGalleryPostPage の hydration を
// 経由するので、frontend の API call → store 反映 → component mount 経路が
// 壊れていないことを smoke level で覆う。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import type { RootFixture } from '../../../fixtures/ui_auth';

test.describe('UI: content detail pages render after API-side create', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(30_000);

  test('navigate to /channels/:id after creating a channel via API', async ({ page, baseURL, request }) => {
    const channelName = `playwright-channel ${Date.now()}`;
    const create = await callApi(request, 'channels/create', {
      i: root.token,
      name: channelName,
    });
    expect(create.status()).toBe(200);
    const channel = await create.json();
    expect(channel.id).toBeTruthy();

    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/channels/${channel.id}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // channel name が body に出る (= MkChannelPage が channels/show を hit)
    await page.waitForFunction(
      (name) => document.body.textContent?.includes(name) ?? false,
      channelName,
      { timeout: 20_000 },
    );
  });

  test('navigate to /clips/:id after creating a clip via API', async ({ page, baseURL, request }) => {
    const clipName = `playwright-clip ${Date.now()}`;
    const create = await callApi(request, 'clips/create', {
      i: root.token,
      name: clipName,
      isPublic: true,
    });
    expect(create.status()).toBe(200);
    const clip = await create.json();
    expect(clip.id).toBeTruthy();

    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/clips/${clip.id}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // clip name が body に出る (= MkClipPage が clips/show を hit)
    await page.waitForFunction(
      (name) => document.body.textContent?.includes(name) ?? false,
      clipName,
      { timeout: 20_000 },
    );
  });

  test('navigate to /gallery/post/:id after creating a gallery post via API', async ({ page, baseURL, request }) => {
    // gallery post は drive file 必須なので、先に drive file を upload する。
    // 簡単化のため既存 file を持たない fresh DB では skip 相当の早期 return
    // にして、本 spec では drive 作成からスタート。
    const driveResp = await request.post(`${baseURL}/api/drive/files/create`, {
      ignoreHTTPSErrors: true,
      multipart: {
        i: root.token,
        // 1x1 transparent PNG (= valid image だが size 最小)
        file: {
          name: 'pixel.png',
          mimeType: 'image/png',
          buffer: Buffer.from(
            'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=',
            'base64',
          ),
        },
      },
    });
    expect(driveResp.status()).toBe(200);
    const driveFile = await driveResp.json();
    expect(driveFile.id).toBeTruthy();

    const galleryTitle = `playwright-gallery ${Date.now()}`;
    const create = await callApi(request, 'gallery/posts/create', {
      i: root.token,
      title: galleryTitle,
      description: 'playwright test gallery post',
      fileIds: [driveFile.id],
      isSensitive: false,
    });
    expect(create.status()).toBe(200);
    const post = await create.json();
    expect(post.id).toBeTruthy();

    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/gallery/${post.id}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (title) => document.body.textContent?.includes(title) ?? false,
      galleryTitle,
      { timeout: 20_000 },
    );
  });
});
