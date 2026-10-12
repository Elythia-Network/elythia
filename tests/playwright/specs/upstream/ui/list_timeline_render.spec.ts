/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// user-list を作成 → member 追加 → member の note を /timeline/list/:id で
// render できることを verify する mixed e2e。/list/:id (= list info / member
// 編集) と /timeline/list/:id (= list の TL) は別 route なので注意。
//
// MkPagination + MkTimeline + Paginator chain で hydrate する。API spec
// (specs/users/user_list.spec.ts) は user-list-timeline endpoint を直接
// callApi で叩く形だが、本 spec は frontend pipeline 全体を smoke する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, signupUser } from '../../../fixtures/auth';
import { resetRateLimit } from '../../../fixtures/rate_limit';
import { pollForTimelineNote } from '../../../fixtures/timeline';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /timeline/list/:id renders user-list-timeline notes', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('owner sees member note in /timeline/list/:id after pushing member + posting note', async ({ page, baseURL, request }) => {
    const memberName = `lstmem${Date.now().toString().slice(-9)}`;
    const member = await signupUser(request, memberName, DEFAULT_TEST_PASSWORD);

    // root が user-list を作成
    const listResp = await callApi(request, 'users/lists/create', {
      i: root.token,
      name: `playwright-list ${Date.now()}`,
    });
    expect(listResp.status()).toBe(200);
    const list = await listResp.json();

    // member を list に push (= follow も自動で発火しないので別途 follow も必要)
    const followResp = await callApi(request, 'following/create', {
      i: root.token,
      userId: member.id,
    });
    expect(followResp.status()).toBe(200);

    const pushResp = await callApi(request, 'users/lists/push', {
      i: root.token,
      listId: list.id,
      userId: member.id,
    });
    expect(pushResp.status()).toBe(204);

    // member が public note を post (= list-timeline に流れる)
    const noteText = `playwright-list-note ${Date.now()}`;
    const noteResp = await callApi(request, 'notes/create', {
      i: member.token,
      text: noteText,
      visibility: 'public',
    });
    expect(noteResp.status()).toBe(200);
    const noteBody = await noteResp.json();
    const noteId = noteBody.createdNote.id;

    // list-timeline は fanout で非同期に積まれるので、UI 表示前に API で
    // 反映完了を polling 待ち (= specs/users/user_list.spec.ts と同 pattern)
    await pollForTimelineNote(request, 'notes/user-list-timeline', root.token, noteId, {
      listId: list.id,
    });

    // root として /list/:id を開いて note text が render されること
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/timeline/list/${list.id}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      noteText,
      { timeout: 20_000 },
    );
  });
});
