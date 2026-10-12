/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #2735: 通知に埋め込まれる note の `files` が解決されていることを、router を
// 組み立てた実サーバー越しに確認する。
//
// **これは配線そのものを守る spec。** `files` は packer が埋めるフィールドでは
// なく、後段の entity.NoteFieldResolver が fileIds から drive_file を引いて
// 埋める二段構えになっている。resolver を通し忘れると fileIds はあるのに
// files が空配列で返り、通知ページの reply / mention / quote (MkNote で全体
// 描画される) から添付メディアが消える。
//
// Go 側の unit test は handler / publisher に resolver を自分で注入するので、
// internal/server の配線が外れても緑のまま通る (internal/server は CI の
// カバレッジ閾値 0% 例外)。REST と streaming の両経路をここで押さえる。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { uploadTinyPNG } from '../../../../fixtures/files';
import { type NotificationBody, pollForNotification } from '../../../../fixtures/notifications';
import { resetRateLimit } from '../../../../fixtures/rate_limit';
import {
  awaitChannelEvent,
  awaitSubscribeBuffer,
  openStream,
  subscribeChannel,
} from '../../../../fixtures/streaming';

// NotificationWithNote は本 spec が見る範囲だけを narrow した shape。
interface NotificationWithNote extends NotificationBody {
  note?: {
    fileIds?: string[];
    files?: { id: string }[];
  };
}

test.describe('notifications: embedded note files', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('mention notification carries the attached drive file', async ({ request, baseURL }) => {
    if (!baseURL) throw new Error('baseURL is required');
    const me = await signupUser(request, randomUsername('nfA'));
    const sender = await signupUser(request, randomUsername('nfB'));
    const file = await uploadTinyPNG(request, baseURL, sender.token, 'notif-attach.png');

    const ws = await openStream(me.token);
    const subId = subscribeChannel(ws, 'main');
    const notifEventPromise = awaitChannelEvent<NotificationWithNote>(
      ws,
      (env) =>
        env.id === subId && env.type === 'notification' && env.body?.type === 'mention',
    );
    await awaitSubscribeBuffer();

    // fileIds は fixtures/notes.ts の helper が扱わない (「特殊な field は直接
    // callApi で送る」方針) ので、既存の note_with_file.spec.ts と同じく直叩きする。
    const created = await callApi(request, 'notes/create', {
      i: sender.token,
      text: `hi @${me.username}`,
      visibility: 'public',
      fileIds: [file.id],
    });
    expect(created.status()).toBe(200);

    // streaming 経路。
    const evNotif = await notifEventPromise;
    expect(evNotif.note?.fileIds).toEqual([file.id]);
    expect(evNotif.note?.files?.map((f) => f.id)).toEqual([file.id]);

    ws.close();

    // REST 経路。
    const httpNotif = (await pollForNotification(
      request,
      me.token,
      (n) => n.type === 'mention' && n.userId === sender.id,
    )) as NotificationWithNote;
    expect(httpNotif.note?.fileIds).toEqual([file.id]);
    expect(httpNotif.note?.files?.map((f) => f.id)).toEqual([file.id]);
  });
});
