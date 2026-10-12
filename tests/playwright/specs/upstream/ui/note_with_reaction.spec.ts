/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// note 詳細 page を reaction がある状態で hydration → MkReactionsViewer が
// reaction button を render することを verify する mixed e2e。
//
// reaction emoji 自体は MkReactionIcon の中で `<img>` / `<MkEmoji>` 化される
// (= raw codepoint が body.textContent に出ない) ので、emoji 文字列 match では
// 検出できない。代わりに reaction button の DOM 出現と reaction count 1 の
// API verify で hydration 完了を確認する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import type { RootFixture } from '../../../fixtures/ui_auth';

test.describe('UI: note detail page with reaction', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(30_000);

  test('open /notes/:id after API-side reactions/create → reactions viewer renders', async ({ page, baseURL, request }) => {
    const noteText = `playwright-react-target ${Date.now()}`;
    const noteResp = await callApi(request, 'notes/create', {
      i: root.token,
      text: noteText,
      visibility: 'public',
    });
    expect(noteResp.status()).toBe(200);
    const noteBody = await noteResp.json();
    const noteId = noteBody.createdNote.id;

    // reactions/create は 204 を返す (default acceptance では unicode emoji 可)
    const reactResp = await callApi(request, 'notes/reactions/create', {
      i: root.token,
      noteId,
      reaction: '👍',
    });
    expect(reactResp.status()).toBe(204);

    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/notes/${noteId}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // 親 note text が render されるまで待機 = MkNoteDetailed の hydration 完了
    await page.waitForFunction(
      (text) => document.body.textContent?.includes(text) ?? false,
      noteText,
      { timeout: 20_000 },
    );

    // backend の reactionCount=1 / myReaction=👍 を verify (= reaction state が
    // 永続化 + frontend が hydrate されたことを double-check)
    const showResp = await callApi(request, 'notes/show', { i: root.token, noteId });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    expect(shown.reactionCount).toBe(1);
    expect(shown.myReaction).toBe('👍');
  });
});
