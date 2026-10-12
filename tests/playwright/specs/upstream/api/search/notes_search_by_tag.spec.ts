/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #828: notes/search-by-tag の round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - notes/search-by-tag { tag } で note.tags 列に tag を含む note を返す
//   - tag 抽出は note 作成時に sync で text/cw から `#tag` を拾い tags 列に
//     格納される (= mk-go では NoteCreateService の hashtag.Extract、
//     #655)。よって poll 不要で post 直後に検索可能。
//   - tag が空または query 配列が空のときは 400 INVALID_PARAM
//
// 本 spec は両 backend 共通で:
//   1. user signup
//   2. unique tag を含む public note を投稿 (text に `#tag` 形式)
//   3. notes/search-by-tag { tag: <unique> } で hit、note.id が一致
//   4. 不存在 tag は空配列
//
// 関連: hashtags/search (= hashtag テーブル経由の prefix 検索) は
// hashtagHook が async (fire-and-forget goroutine) で populate するため
// flaky 化リスクあり、別 spec PR scope。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface SearchedNote {
  id: string;
}

test.describe('search: notes/search-by-tag round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('search by hashtag returns the tagged note', async ({ request }) => {
    const author = await signupUser(request, randomUsername('btA'));
    // tag は既存 stack の他 note と衝突しないよう uniq 化。
    // hashtag.Extract は alphanumeric / underscore を許容、ms timestamp の
    // base36 representation で十分 short かつ uniq。
    const tag = `pwtag${Date.now().toString(36)}`;
    const note = await createNote(request, author.token, {
      text: `tag this note with #${tag} please`,
      visibility: 'public',
    });

    const resp = await callApi(request, 'notes/search-by-tag', {
      i: author.token,
      tag,
      limit: 10,
    });
    expect(resp.status()).toBe(200);
    const list = (await resp.json()) as SearchedNote[];
    expect(Array.isArray(list)).toBe(true);
    const hit = list.find((n) => n.id === note.id);
    expect(hit).toBeDefined();
  });

  test('non-existent tag returns empty array', async ({ request }) => {
    const me = await signupUser(request, randomUsername('btB'));
    const resp = await callApi(request, 'notes/search-by-tag', {
      i: me.token,
      tag: `noexist_${Date.now().toString(36)}`,
      limit: 10,
    });
    expect(resp.status()).toBe(200);
    const list = (await resp.json()) as SearchedNote[];
    expect(list).toEqual([]);
  });

  test('empty tag is rejected with 400', async ({ request }) => {
    const me = await signupUser(request, randomUsername('btC'));
    const resp = await callApi(request, 'notes/search-by-tag', {
      i: me.token,
      tag: '',
      limit: 10,
    });
    expect(resp.status()).toBe(400);
  });
});
