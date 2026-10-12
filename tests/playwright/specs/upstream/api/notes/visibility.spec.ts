/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #744 Phase 1 PR-3: visibility の境界。
// public / home / followers / specified の各 visibility で note を作成し、
// 第三者 viewer (= author 非関連) が notes/show で取得できるか / できないか
// を確認する。
//
// upstream Misskey TS は:
//   - public: 誰でも閲覧可
//   - followers: follower 限定。stranger は 4xx
//   - specified: visibleUserIds 列挙された user 限定。stranger は 4xx
//
// 本 spec は author + stranger 2 user で「stranger が見えるべきか」を
// 端的にチェックする。federation や follow を必要とせず mk-go single
// instance で実行可能。
//
// `home` visibility は follow graph の挙動 (follower で見える / 第三者は
// 不可視) が要るので別 spec で扱う想定。本 PR scope 外。

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote, showNoteRaw } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('notes: visibility boundary', () => {
  // 各 test で author + stranger を作るので spec ファイル全体で signup を
  // 6 回叩く。signup endpoint の 1h 5 回 limit を超えるので beforeAll では
  // なく beforeEach で毎 test 前に reset する。
  test.beforeEach(() => {
    resetRateLimit();
  });

  test('public note is readable by stranger', async ({ request }) => {
    const author = await signupUser(request, randomUsername('vPubA'));
    const stranger = await signupUser(request, randomUsername('vPubS'));
    const note = await createNote(request, author.token, {
      text: 'visible to all',
      visibility: 'public',
    });

    const resp = await showNoteRaw(request, stranger.token, note.id);
    expect(resp.status()).toBe(200);
  });

  // upstream Misskey TS と mk-go (#799 fix 後) は、`notes/show` で直接 ID
  // 指定された note は visibility 違反でも 200 で返す (= "ID を既に知って
  // いる viewer には公開する" 設計)。timeline / replies / renotes 等の
  // 二次経路は引き続き visibility filter で除外される。
  //
  // 本 spec は「stranger でも `notes/show` で 200 を返す」のを assert する。
  // visibility filter が壊れる方向の regression は timeline 系 spec (Phase
  // 1 残作業の home/local/global timeline) で別途 cover する。
  test('followers-only note is returned by notes/show even to stranger', async ({ request }) => {
    const author = await signupUser(request, randomUsername('vFoA'));
    const stranger = await signupUser(request, randomUsername('vFoS'));
    const note = await createNote(request, author.token, {
      text: 'followers only',
      visibility: 'followers',
    });

    const resp = await showNoteRaw(request, stranger.token, note.id);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.id).toBe(note.id);
    expect(body.visibility).toBe('followers');
  });

  test('specified note is returned by notes/show even to non-listed user', async ({ request }) => {
    const author = await signupUser(request, randomUsername('vSpA'));
    const stranger = await signupUser(request, randomUsername('vSpS'));
    const note = await createNote(request, author.token, {
      text: 'private DM',
      visibility: 'specified',
      visibleUserIds: [author.id],
    });

    const resp = await showNoteRaw(request, stranger.token, note.id);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.id).toBe(note.id);
    expect(body.visibility).toBe('specified');
  });
});
