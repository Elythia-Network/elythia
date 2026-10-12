/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /clips/:id で clips/add-note 経由で追加した note text が body に出る
// ことを verify する mixed e2e。
//
// content_pages.spec.ts では空 clip の名前 render を verify するが、本 spec
// は clip 内 note timeline の hydration (= MkClipPage の clips/notes
// paginator chain) まで covers する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import type { RootFixture } from '../../../fixtures/ui_auth';

test.describe('UI: /clips/:id renders notes added to the clip', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(30_000);

  test('clip detail page renders an added note', async ({ page, baseURL, request }) => {
    // note + clip を作成
    const noteText = `pwclipnote-${Date.now().toString().slice(-9)}`;
    const noteResp = await callApi(request, 'notes/create', {
      i: root.token,
      text: noteText,
      visibility: 'public',
    });
    expect(noteResp.status()).toBe(200);
    const noteId = (await noteResp.json()).createdNote.id;

    const clipName = `pwclip-${Date.now().toString().slice(-9)}`;
    const clipResp = await callApi(request, 'clips/create', {
      i: root.token,
      name: clipName,
      isPublic: true,
    });
    expect(clipResp.status()).toBe(200);
    const clip = await clipResp.json();
    expect(clip.id).toBeTruthy();

    // clip に note を追加
    const addResp = await callApi(request, 'clips/add-note', {
      i: root.token,
      clipId: clip.id,
      noteId,
    });
    expect(addResp.status()).toBe(204);

    // /clips/:id を navigate して clip name + note text の両方が render される
    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/clips/${clip.id}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      clipName,
      { timeout: 20_000 },
    );
    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      noteText,
      { timeout: 20_000 },
    );
  });
});
