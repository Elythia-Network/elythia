/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #882: admin/emoji bulk operations の round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - admin/emoji/add-aliases-bulk { ids, aliases }: 各 emoji の alias に
//     新規 alias を merge (重複は dedupe)
//   - admin/emoji/remove-aliases-bulk { ids, aliases }: 各 emoji の alias
//     から指定 alias を除外
//   - admin/emoji/set-aliases-bulk { ids, aliases }: 全 ids の alias を
//     同じ aliases に上書き
//   - admin/emoji/set-category-bulk { ids, category }: 全 ids の category
//     を同じ category に上書き
//   - admin/emoji/set-license-bulk { ids, license }: 全 ids の license を
//     同じ license に上書き
//   - admin/emoji/delete-bulk { ids }: 全 ids を delete
//
// 本 spec は 2 emoji を作成し、上記 6 endpoint の round-trip を 1 test で
// 順次走らせる (= setup の重複を避けて test 全体の所要時間を抑える)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { tinyPNG } from '../../../../fixtures/files';
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
  category?: string | null;
  aliases?: string[];
  license?: string | null;
}

async function uploadEmoji(
  request: import('@playwright/test').APIRequestContext,
  token: string,
  name: string,
): Promise<string> {
  const upload = await request.post(`${baseURL}/api/drive/files/create`, {
    multipart: {
      i: token,
      file: { name: `${name}.png`, mimeType: 'image/png', buffer: tinyPNG },
    },
    failOnStatusCode: false,
  });
  expect(upload.status()).toBe(200);
  const uploaded = (await upload.json()) as { id: string };
  const add = await callApi(request, 'admin/emoji/add', {
    i: token,
    name,
    fileId: uploaded.id,
  });
  expect(add.status()).toBe(200);
  const added = (await add.json()) as { id: string };
  return added.id;
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

test.describe('emoji: admin bulk operations', () => {
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

  test('bulk alias / category / license / delete round-trip', async ({
    request,
  }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    rootToken = root.token;
    const suffix = Math.random().toString(16).slice(2, 8);
    const name1 = `bulk1_${suffix}`;
    const name2 = `bulk2_${suffix}`;

    // 2 emoji を作成 (upload + add)。
    const id1 = await uploadEmoji(request, root.token, name1);
    const id2 = await uploadEmoji(request, root.token, name2);
    createdIds = [id1, id2];

    // add-aliases-bulk → 両 emoji に "alpha" / "beta" alias が merge される
    {
      const resp = await callApi(request, 'admin/emoji/add-aliases-bulk', {
        i: root.token,
        ids: [id1, id2],
        aliases: ['alpha', 'beta'],
      });
      expect([200, 204]).toContain(resp.status());
      const e1 = await findEmojiByName(request, root.token, name1);
      const e2 = await findEmojiByName(request, root.token, name2);
      expect(e1?.aliases).toEqual(expect.arrayContaining(['alpha', 'beta']));
      expect(e2?.aliases).toEqual(expect.arrayContaining(['alpha', 'beta']));
    }

    // remove-aliases-bulk → "alpha" だけ除外、"beta" は残る
    {
      const resp = await callApi(request, 'admin/emoji/remove-aliases-bulk', {
        i: root.token,
        ids: [id1, id2],
        aliases: ['alpha'],
      });
      expect([200, 204]).toContain(resp.status());
      const e1 = await findEmojiByName(request, root.token, name1);
      expect(e1?.aliases).not.toContain('alpha');
      expect(e1?.aliases).toContain('beta');
    }

    // set-aliases-bulk → alias を ["gamma"] で上書き (= 既存 "beta" は消える)
    {
      const resp = await callApi(request, 'admin/emoji/set-aliases-bulk', {
        i: root.token,
        ids: [id1, id2],
        aliases: ['gamma'],
      });
      expect([200, 204]).toContain(resp.status());
      const e1 = await findEmojiByName(request, root.token, name1);
      expect(e1?.aliases).toEqual(['gamma']);
    }

    // set-category-bulk → 全件同じ category に上書き
    {
      const cat = `bulk-cat-${suffix}`;
      const resp = await callApi(request, 'admin/emoji/set-category-bulk', {
        i: root.token,
        ids: [id1, id2],
        category: cat,
      });
      expect([200, 204]).toContain(resp.status());
      const e1 = await findEmojiByName(request, root.token, name1);
      const e2 = await findEmojiByName(request, root.token, name2);
      expect(e1?.category).toBe(cat);
      expect(e2?.category).toBe(cat);
    }

    // set-license-bulk → 全件同じ license に上書き
    {
      const lic = `CC-BY-${suffix}`;
      const resp = await callApi(request, 'admin/emoji/set-license-bulk', {
        i: root.token,
        ids: [id1, id2],
        license: lic,
      });
      expect([200, 204]).toContain(resp.status());
      const e1 = await findEmojiByName(request, root.token, name1);
      expect(e1?.license).toBe(lic);
    }

    // delete-bulk → 一覧から消える
    {
      const resp = await callApi(request, 'admin/emoji/delete-bulk', {
        i: root.token,
        ids: [id1, id2],
      });
      expect([200, 204]).toContain(resp.status());
      // delete 済 = afterEach の cleanup は不要
      createdIds = [];
      const e1 = await findEmojiByName(request, root.token, name1);
      const e2 = await findEmojiByName(request, root.token, name2);
      expect(e1).toBeUndefined();
      expect(e2).toBeUndefined();
    }
  });
});
