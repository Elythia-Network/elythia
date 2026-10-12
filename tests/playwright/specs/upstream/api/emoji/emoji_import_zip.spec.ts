/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #882: admin/emoji/import-zip の async round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   1. ZIP を drive/files/create で upload (= fileId 取得)
//   2. admin/emoji/import-zip { fileId } で job を enqueue (204)
//   3. queue worker が ZIP を展開し meta.json に従って各 emoji を
//      admin/emoji/add 相当で local custom emoji に登録
//   4. admin/emoji/list の query で各 emoji が現れる
//
// 即時反映ではないので polling で最大 30 秒待つ。notification spec の
// pollForNotification と同型で、queue 処理時間 + drive store + emoji
// repo write を見込んで余裕を持つ。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { buildEmojiImportZip } from '../../../../fixtures/files';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

const baseURL = process.env.MK_BASE_URL ?? 'https://mkgo.local';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

interface AdminEmoji {
  id: string;
  name: string;
}

async function findEmojiByName(
  request: import('@playwright/test').APIRequestContext,
  token: string,
  name: string,
): Promise<AdminEmoji | undefined> {
  const list = await callApi(request, 'admin/emoji/list', {
    i: token,
    query: name,
    limit: 10,
  });
  expect(list.status()).toBe(200);
  const body = (await list.json()) as AdminEmoji[];
  return body.find((e) => e.name === name);
}

// pollForEmoji waits until the named emoji appears in admin/emoji/list, with
// a bounded retry loop. queue 処理 + drive write + emoji repo insert で
// 数秒かかるため最大 30 秒で 0.5 秒間隔の polling とする (notification の
// 5 秒より長め推奨、issue #882 の指示通り)。
async function pollForEmoji(
  request: import('@playwright/test').APIRequestContext,
  token: string,
  name: string,
): Promise<AdminEmoji | undefined> {
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    const e = await findEmojiByName(request, token, name);
    if (e) return e;
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  return undefined;
}

test.describe('emoji: admin import-zip async round-trip', () => {
  // TS backend は queue worker が drive URL を got で fetch するが
  // got は NODE_TLS_REJECT_UNAUTHORIZED env を honor せず self-signed cert を
  // 拒否するため、このスタックでは ZIP の download phase で常に失敗する。
  // mk-go の HTTP fetch は InsecureSkipVerify を尊重するので動作する。
  // backend 改修とは無関係な test infra の constraint なので、TS では skip。
  test.skip(
    process.env.MK_BACKEND_TYPE === 'ts',
    'TS の queue worker は self-signed HTTPS から drive を fetch できない (test infra constraint)',
  );

  let createdIds: string[] = [];
  let rootToken: string | undefined;

  test.beforeAll(() => {
    resetRateLimit();
  });

  test.afterEach(async ({ request }) => {
    if (rootToken && createdIds.length > 0) {
      await callApi(request, 'admin/emoji/delete-bulk', {
        i: rootToken,
        ids: createdIds,
      });
    }
    createdIds = [];
    rootToken = undefined;
  });

  test('import-zip enqueues job, queue worker registers emojis, list reflects', async ({
    request,
  }) => {
    test.setTimeout(60_000);

    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    rootToken = root.token;
    const suffix = Math.random().toString(16).slice(2, 8);
    const name1 = `imp1_${suffix}`;
    const name2 = `imp2_${suffix}`;

    // 2 emoji を含む ZIP を構築 (= meta.json + 2 PNG)。
    const zipBuf = buildEmojiImportZip([{ name: name1 }, { name: name2 }]);

    // ZIP を drive/files/create で upload。
    const upload = await request.post(`${baseURL}/api/drive/files/create`, {
      multipart: {
        i: root.token,
        file: {
          name: `emoji_import_${suffix}.zip`,
          mimeType: 'application/zip',
          buffer: zipBuf,
        },
      },
      failOnStatusCode: false,
    });
    expect(upload.status()).toBe(200);
    const uploaded = (await upload.json()) as { id: string };
    expect(typeof uploaded.id).toBe('string');

    // import-zip → 204 No Content (= job enqueue 成功)
    const importResp = await callApi(request, 'admin/emoji/import-zip', {
      i: root.token,
      fileId: uploaded.id,
    });
    expect([200, 204]).toContain(importResp.status());

    // queue worker が両 emoji を登録するまで polling
    const e1 = await pollForEmoji(request, root.token, name1);
    const e2 = await pollForEmoji(request, root.token, name2);
    expect(e1).toBeDefined();
    expect(e2).toBeDefined();
    expect(e1!.name).toBe(name1);
    expect(e2!.name).toBe(name2);
    createdIds = [e1!.id, e2!.id];
  });
});
