/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/antennas/:id の MkAntennaEditor inline Delete button (inline danger
// + ti-trash icon) → confirm OK → /api/antennas/delete round-trip する
// write-flow spec。
//
// MkAntennaEditor.vue:41 の MkButton inline danger は ti-trash + "Delete"
// text、click すると os.confirm warning → 承諾後 antennas/delete を叩く
// (line 177)。antenna_edit_save spec の sister として、削除 path を strict
// 化する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { NOT_FOUND_STATUS } from '../../../fixtures/backend';
import { clickButtonWithIcon, clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: /my/antennas/:id delete button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('Delete button → confirm OK → /api/antennas/delete', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 antenna を create via API
    const antennaName = `pw-ant-del-${Date.now()}`;
    const createResp = await callApi(request, 'antennas/create', {
      i: root.token,
      name: antennaName,
      src: 'all',
      keywords: [['test']],
      excludeKeywords: [[]],
      users: [],
      caseSensitive: false,
      withReplies: false,
      withFile: false,
      notify: false,
      excludeBots: false,
      localOnly: false,
    });
    expect(createResp.status()).toBe(200);
    const antenna = await createResp.json();
    const antennaId: string = antenna.id;
    expect(antennaId).toBeTruthy();

    // 2. antenna 編集ページを開く
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/antennas/${antennaId}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // antenna name は MkAntennaEditor.vue:10 の `<MkInput v-model="name">`
    // で **input value としてのみ** render されるため `document.body.textContent`
    // に含まれない (input value は attribute、textContent には現れない)。
    // 旧実装は body wait で 20s timeout していた。直接 Delete button (=
    // ti-trash icon を持つ MkButton inline danger) が出るのを待てば
    // MkAntennaEditor mount 完了の verify として十分。
    await clickButtonWithIcon(page, 'i.ti-trash', { timeout: 30_000 });

    // 4. confirm dialog OK click → API 呼出
    await page.waitForFunction(
      () => document.querySelector('[data-testid="modal-dialog-ok"]') !== null,
      { timeout: 10_000 },
    );

    const deleteResp = page.waitForResponse(
      (r) => r.url().includes('/api/antennas/delete') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    await deleteResp;

    // 5. API 経由で削除確認 — antennas/show は 404 + NO_SUCH_ANTENNA を返す
    // (antennas/show の UUID c06569fb-b025-4f23-b22d-1fcd20d2816b)。
    const showResp = await callApi(request, 'antennas/show', {
      i: root.token,
      antennaId,
    });
    expect(showResp.status()).toBe(NOT_FOUND_STATUS);
    const showBody = await showResp.json();
    expect(showBody.error?.code).toBe('NO_SUCH_ANTENNA');
    expect(showBody.error?.id).toBe('c06569fb-b025-4f23-b22d-1fcd20d2816b');
  });
});
