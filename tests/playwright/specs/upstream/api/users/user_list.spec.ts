/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #820: users/lists CRUD + push/pull member + user-list-timeline。
//
// upstream Misskey TS と mk-go は両方とも:
//   - users/lists/create { name } で UserList を作成し entity (id/name/userId)
//     を返す
//   - users/lists/list で自分の list 一覧
//   - users/lists/push { listId, userId } / pull で member 操作 (204)
//   - users/lists/show { listId } で list を返す
//   - users/lists/delete { listId } で削除 (204)
//   - notes/user-list-timeline { listId } で member の note を timeline 形式
//     で取得 (= upstream は user_list_membership と JOIN、mk-go も同様)
//
// 本 spec は両 backend 共通で:
//   1. owner / member signup
//   2. owner が user list を作成 / list 一覧で含むこと
//   3. owner が member を push
//   4. member が public note 投稿
//   5. owner の user-list-timeline で note を pollForTimelineNote (fanout
//      が settle するまで poll)
//   6. owner が member を pull → 削除確認
//   7. owner が list を delete → users/lists/list から消える

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';
import { pollForTimelineNote } from '../../../../fixtures/timeline';

// drop-in shape drift (= #871 で揃える方向):
//   - TS の users/lists/create は { id, createdAt, name, userIds[], isPublic }
//   - mk-go は { id, userId, name }
//   両者で確実に存在するのは id / name のみなので spec 側はそこだけ assert
//   する。
interface UserList {
  id: string;
  name: string;
}

test.describe('users: list CRUD + member ops + user-list-timeline', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('owner creates list, pushes member, sees their note in user-list-timeline, then pulls and deletes', async ({
    request,
  }) => {
    const owner = await signupUser(request, randomUsername('ulA'));
    const member = await signupUser(request, randomUsername('ulB'));

    // create list。
    const createResp = await callApi(request, 'users/lists/create', {
      i: owner.token,
      name: 'spec-list',
    });
    expect(createResp.status()).toBe(200);
    const list = (await createResp.json()) as UserList;
    expect(list.name).toBe('spec-list');
    // userId / userIds 等の owner 情報 field は backend 間で形が違うので
    // 本 spec では含まず、所有権は users/lists/list の自分視点取得で
    // 間接的に check する (= owner の list 一覧で含まれる = 自分の list)。

    // owner の list 一覧で含むこと。
    const listsResp = await callApi(request, 'users/lists/list', {
      i: owner.token,
    });
    expect(listsResp.status()).toBe(200);
    const lists = (await listsResp.json()) as UserList[];
    expect(lists.some((l) => l.id === list.id)).toBe(true);

    // member を push → 204。
    const pushResp = await callApi(request, 'users/lists/push', {
      i: owner.token,
      listId: list.id,
      userId: member.id,
    });
    expect(pushResp.status()).toBe(204);

    // member が public note 投稿、user-list-timeline で出ることを poll で確認。
    const note = await createNote(request, member.token, {
      text: 'list timeline check',
      visibility: 'public',
    });
    await pollForTimelineNote(
      request,
      'notes/user-list-timeline',
      owner.token,
      note.id,
      { listId: list.id },
    );

    // member を pull → 204。
    const pullResp = await callApi(request, 'users/lists/pull', {
      i: owner.token,
      listId: list.id,
      userId: member.id,
    });
    expect(pullResp.status()).toBe(204);

    // list を delete → 204。
    const deleteResp = await callApi(request, 'users/lists/delete', {
      i: owner.token,
      listId: list.id,
    });
    expect(deleteResp.status()).toBe(204);

    // owner の list 一覧から消えていること。
    const listsAfter = await callApi(request, 'users/lists/list', {
      i: owner.token,
    });
    expect(listsAfter.status()).toBe(200);
    const listsRemaining = (await listsAfter.json()) as UserList[];
    expect(listsRemaining.some((l) => l.id === list.id)).toBe(false);
  });
});
