/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /timeline/antenna/:antennaId で antennas/create で作成した antenna の
// timeline に matching note が render されることを verify する mixed e2e。
//
// antenna は keyword match で local timeline から note を絞り込む機能。
// 1. root が antenna 作成 (keywords=['pwantena'])
// 2. 別 user が public note 投稿 (text 内に keyword 含む)
// 3. /timeline/antenna/:id を navigate → antenna timeline で note text が
//    body に出る (= notes/antennas-timeline endpoint からの hydration)

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, signupUser } from '../../../fixtures/auth';
import { resetRateLimit } from '../../../fixtures/rate_limit';
import { pollForTimelineNote } from '../../../fixtures/timeline';
import { deleteAntennasNamed } from '../../../fixtures/quota';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /timeline/antenna/:id renders matching notes', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  // 作った antenna は片付ける。root を共有しているので放置すると
  // antennaLimit (既定 5) を使い切って無関係な spec が create で落ちる (#2264)。
  const createdAntennas: string[] = [];
  test.afterEach(async ({ request }) => {
    await deleteAntennasNamed(request, root.token, createdAntennas);
    createdAntennas.length = 0;
  });

  test('antenna timeline renders a note that matches keyword', async ({
    page,
    baseURL,
    request,
  }) => {
    // 一意 keyword を使って antenna を作成
    const keyword = `pwant${Date.now().toString().slice(-9)}`;
    const antennaName = `pw-antenna ${Date.now()}`;
    createdAntennas.push(antennaName);
    const antennaResp = await callApi(request, 'antennas/create', {
      i: root.token,
      name: antennaName,
      src: 'all',
      keywords: [[keyword]],
      excludeKeywords: [[]],
      users: [],
      caseSensitive: false,
      withReplies: false,
      withFile: false,
      notify: false,
    });
    expect(antennaResp.status()).toBe(200);
    const antenna = await antennaResp.json();
    expect(antenna.id, 'antennas/create should return id').toBeTruthy();

    // 別 user (= local timeline に流す note 作者) を作って matching note 投稿
    const otherName = `ant${Date.now().toString().slice(-9)}`;
    const other = await signupUser(request, otherName, DEFAULT_TEST_PASSWORD);
    const noteText = `${keyword}-${Date.now().toString().slice(-9)}`;
    const noteResp = await callApi(request, 'notes/create', {
      i: other.token,
      text: noteText,
      visibility: 'public',
    });
    expect(noteResp.status()).toBe(200);
    const noteId = (await noteResp.json()).createdNote.id;

    // antenna match index への反映を polling で待つ (= antennas/notes
    // endpoint で対象 note が引けるようになるまで)
    await pollForTimelineNote(request, 'antennas/notes', root.token, noteId, {
      antennaId: antenna.id,
    });

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/timeline/antenna/${antenna.id}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      noteText,
      { timeout: 20_000 },
    );
  });
});
