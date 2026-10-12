/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// note 詳細 page を画像 file 添付状態で render → MkMediaList が file を
// reflect することを verify する mixed e2e。
//
// drive file は multipart で /api/drive/files/create に upload (= 既存
// content_pages.spec.ts の gallery で使う 1x1 PNG を使い回し)。fileIds 込みで
// notes/create し、/notes/:id を navigate して file の name が body に
// 含まれることを smoke する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import type { RootFixture } from '../../../fixtures/ui_auth';

test.describe('UI: note detail page with attached drive file', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(30_000);

  test('open /notes/:id after notes/create with fileIds → media filename appears', async ({ page, baseURL, request }) => {
    // 1x1 transparent PNG を drive に upload
    const driveResp = await request.post(`${baseURL}/api/drive/files/create`, {
      ignoreHTTPSErrors: true,
      multipart: {
        i: root.token,
        file: {
          name: `pw-note-pixel-${Date.now()}.png`,
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
    expect(driveFile.name).toBeTruthy();

    const noteText = `playwright-note-with-file ${Date.now()}`;
    const noteResp = await callApi(request, 'notes/create', {
      i: root.token,
      text: noteText,
      visibility: 'public',
      fileIds: [driveFile.id],
    });
    expect(noteResp.status()).toBe(200);
    const noteBody = await noteResp.json();
    const noteId = noteBody.createdNote.id;

    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/notes/${noteId}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (text) => document.body.textContent?.includes(text) ?? false,
      noteText,
      { timeout: 20_000 },
    );

    // backend 側で files が attach されている (= notes/show 経由) ことを verify。
    // shown.files[0].url は drive file の actual URL (= file.accessKey ベースで
    // construct、mk-go では `/files/<accessKey>`)。drive file の id (= ULID)
    // と accessKey (= 32 hex) は別物なので id では match しない。
    const showResp = await callApi(request, 'notes/show', { i: root.token, noteId });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    expect(Array.isArray(shown.files), 'files should be array').toBe(true);
    expect(shown.files.length, 'note should have 1 file attached').toBe(1);
    expect(shown.files[0].id).toBe(driveFile.id);

    // MkMediaImage は原寸 (file.url) ではなく **thumbnailUrl** を <img src>
    // に載せる。mk-go の thumbnail は原寸とは別 accessKey で保存されるので
    // `/files/<accessKey>` は url と一致しない。かつ現行の MkMediaImage は
    // <a href> ではなく <button> + image viewer なので href 照合も効かない。
    // よって url / thumbnailUrl どちらかの pathname が <img src> か <a href>
    // に現れることを条件にする (proxy / 直接配信 両 shape OK)。
    const fileUrl: string = shown.files[0].url;
    expect(typeof fileUrl, 'file.url should be a string').toBe('string');
    const candidatePaths = [fileUrl, shown.files[0].thumbnailUrl]
      .filter((u): u is string => typeof u === 'string' && u.length > 0)
      .map((u) => new URL(u).pathname);
    await page.waitForFunction(
      (paths) =>
        paths.some(
          (path) =>
            Array.from(document.querySelectorAll('a')).some((a) => a.href.includes(path)) ||
            Array.from(document.querySelectorAll('img')).some((img) => img.src.includes(path)),
        ),
      candidatePaths,
      { timeout: 20_000 },
    );
  });
});
