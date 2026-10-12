/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #828: notes/search の text 検索 round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - notes/search { query } で text 検索を実行
//   - 空 query は 400 INVALID_PARAM で reject
//
// drop-in capability drift (= 後続 issue で揃える方向):
//   - upstream Misskey TS は meilisearch / sonic 等の external search backend
//     を必須とし、未配備時は 400 UNAVAILABLE を返す
//   - mk-go は SQL LIKE による fallback path を持つので search backend 無しでも
//     200 + 結果を返す
//   playwright stack には search backend を組み込まない方針なので、本 spec は:
//     - 200 (mk-go の SQL LIKE 結果) → hit を strict assert
//     - 400 UNAVAILABLE (TS 側) → backend 不在として許容
//   両 case を許容する。capability gap の解消 (= mk-go SQL LIKE は upstream
//   非互換、または playwright stack に meilisearch 相当を組み込み) は
//   別 issue で扱う scope。
//
// 本 spec は両 backend 共通で:
//   1. user signup
//   2. unique 文字列を含む public note を投稿
//   3. notes/search で 200 なら hit を、400 なら UNAVAILABLE を確認
//   4. 空 query は 400

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface SearchedNote {
  id: string;
  text?: string | null;
}

test.describe('search: notes/search text round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('search by unique substring returns the matching note', async ({
    request,
  }) => {
    const author = await signupUser(request, randomUsername('nsA'));
    // 衝突しないように user prefix と timestamp 由来の suffix を combine
    // (= 既存 stack 上に同 query で hit する note があっても本 test note は
    // 必ず単独で識別できる文字列にする)。
    const unique = `pwsearch_${Date.now()}`;
    const note = await createNote(request, author.token, {
      text: `hello ${unique} world`,
      visibility: 'public',
    });

    const resp = await callApi(request, 'notes/search', {
      i: author.token,
      query: unique,
      limit: 10,
    });
    if (resp.status() === 200) {
      // mk-go SQL LIKE fallback 等、search backend が動く場合
      // unique 文字列で必ず hit し、text shape も regression guard する
      // (= packNote が text を含む shape を返すこと)。
      const list = (await resp.json()) as SearchedNote[];
      expect(Array.isArray(list)).toBe(true);
      const hit = list.find((n) => n.id === note.id);
      expect(hit).toBeDefined();
      expect(hit?.text).toContain(unique);
    } else {
      // upstream Misskey TS 等 external search backend 必須環境では
      // 400 UNAVAILABLE を返す (= playwright stack に meilisearch 不在)。
      expect(resp.status()).toBe(400);
      const body = await resp.json();
      expect(body.error?.code).toBe('UNAVAILABLE');
    }
  });

  test('empty query is rejected with 400', async ({ request }) => {
    const me = await signupUser(request, randomUsername('nsB'));
    const resp = await callApi(request, 'notes/search', {
      i: me.token,
      query: '',
      limit: 10,
    });
    expect(resp.status()).toBe(400);
  });
});
