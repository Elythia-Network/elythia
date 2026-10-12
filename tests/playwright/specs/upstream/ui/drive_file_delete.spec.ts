/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/drive/file/:fileId で trash button を click → confirm dialog OK →
// /api/drive/files/delete が round-trip する write-flow spec。
//
// drive.file.info.vue の deleteFile() は os.confirm warning → 承諾後に
// drive/files/delete を叩く。ボタンは fileQuickActionsOthers 内、ti-trash
// アイコン + danger style。post / sensitive ボタンと違い trash は唯一の
// danger color。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { uploadTinyPNG } from '../../../fixtures/files';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { NOT_FOUND_STATUS } from '../../../fixtures/backend';
import { clickButtonWithIcon, clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: /my/drive/file/:fileId delete flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('trash button → confirm OK → /api/drive/files/delete', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 file を upload
    const file = await uploadTinyPNG(request, baseURL!, root.token, `pw-del-${Date.now()}.png`);
    const fileId = file.id;

    // 2. detail page を開いて hydrate を待つ
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/drive/file/${fileId}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // trash button (= ti-trash icon を持つ button) hydrate を待つ
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some((b) => b.querySelector('i.ti-trash') !== null);
      },
      { timeout: 20_000 },
    );

    // 3. trash click → confirm dialog 出現
    await clickButtonWithIcon(page, 'i.ti-trash');

    await page.waitForFunction(
      () => document.querySelector('[data-testid="modal-dialog-ok"]') !== null,
      { timeout: 10_000 },
    );

    // 4. OK → drive/files/delete round-trip
    const deleteResp = page.waitForResponse(
      (r) => r.url().includes('/api/drive/files/delete') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    await deleteResp;

    // 5. API 経由で 削除確認 — drive/files/show は削除済 file に対して
    // 404 + NO_SUCH_FILE error code + 固定 UUID を返す (handler.go:466
    // 確認)。code に加えて UUID も verify することで code rename 系の
    // refactor regression も catch する。UUID は upstream Misskey TS と
    // 一致する Misskey-compat ID なので drift 検出にも有効。
    const showResp = await request.post(`${baseURL}/api/drive/files/show`, {
      ignoreHTTPSErrors: true,
      data: { i: root.token, fileId },
    });
    expect(showResp.status()).toBe(NOT_FOUND_STATUS);
    const showBody = await showResp.json();
    expect(showBody.error?.code).toBe('NO_SUCH_FILE');
    expect(showBody.error?.id).toBe('067bc436-2718-4795-b0fb-ecbe43949e31');
  });
});
