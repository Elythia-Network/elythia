/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-A: channels/* full round-trip。
//
// channels/* (16 endpoint) を 1 round-trip で纏めて exercise する。
//
//   1. create → 自分が作った channel が返る
//   2. show → 同 id で取得して shape 一致
//   3. update → name / description 変更
//   4. search で query にマッチ
//   5. featured (anonymous 可) で list shape
//   6. owned で自分の channel 含まれる
//   7. follow → 204
//   8. followed で自分が follow した channel 含まれる
//   9. timeline (channelId 必須) で list shape
//  10. favorite → 204 / my-favorites で含まれる / unfavorite → 204
//  11. mute/create → 204 / mute/list で含まれる / mute/delete → 204
//  12. unfollow → 204
//
// すべて両 backend (TS / mk-go) で同 shape + status を返すこと前提の LCD spec。

import { randomUUID } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface Channel {
  id: string;
  name: string;
  description?: string;
  isFollowing?: boolean;
  isFavorited?: boolean;
}

test.describe('channels/* full round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('create → show → update → search → featured → owned → follow → favorite → mute → unfollow round-trip', async ({
    request,
  }) => {
    const me = await signupUser(request, randomUsername('ch'));
    // 一意な name で他 spec / 残存 record と分離。検索で hit する long suffix
    // を含める (channels/search はサブストリング検索)。
    const baseName = `spec_${randomUUID()}`;

    // 1. create
    const createResp = await callApi(request, 'channels/create', {
      i: me.token,
      name: baseName,
      description: 'phase4 spec',
    });
    expect(createResp.status()).toBe(200);
    const created = (await createResp.json()) as Channel;
    expect(typeof created.id).toBe('string');
    expect(created.name).toBe(baseName);
    const channelId = created.id;

    // 2. show
    const showResp = await callApi(request, 'channels/show', { channelId });
    expect(showResp.status()).toBe(200);
    const shown = (await showResp.json()) as Channel;
    expect(shown.id).toBe(channelId);

    // 3. update
    const updResp = await callApi(request, 'channels/update', {
      i: me.token,
      channelId,
      description: 'updated by spec',
    });
    expect(updResp.status()).toBe(200);
    const updated = (await updResp.json()) as Channel;
    expect(updated.description).toBe('updated by spec');

    // 4. search で hit (= 一意 name)
    const searchResp = await callApi(request, 'channels/search', {
      query: baseName,
      limit: 10,
    });
    expect(searchResp.status()).toBe(200);
    const searchList = (await searchResp.json()) as Channel[];
    expect(Array.isArray(searchList)).toBe(true);
    expect(searchList.find((c) => c.id === channelId)).toBeDefined();

    // 5. featured (anonymous 可) shape
    const featResp = await callApi(request, 'channels/featured', { limit: 5 });
    expect(featResp.status()).toBe(200);
    expect(Array.isArray(await featResp.json())).toBe(true);

    // 6. owned で自分の channel
    const ownedResp = await callApi(request, 'channels/owned', { i: me.token });
    expect(ownedResp.status()).toBe(200);
    const owned = (await ownedResp.json()) as Channel[];
    expect(owned.find((c) => c.id === channelId)).toBeDefined();

    // 7. follow → 204
    const followResp = await callApi(request, 'channels/follow', {
      i: me.token,
      channelId,
    });
    expect(followResp.status()).toBe(204);

    // 8. followed で含まれる
    const followedResp = await callApi(request, 'channels/followed', {
      i: me.token,
    });
    expect(followedResp.status()).toBe(200);
    const followed = (await followedResp.json()) as Channel[];
    expect(followed.find((c) => c.id === channelId)).toBeDefined();

    // 8b. show 再取得で viewer-aware field (isFollowing) が反映されている
    //     ことを verify (= follow が状態として観測できる)。
    const showAfterFollow = await callApi(request, 'channels/show', {
      i: me.token,
      channelId,
    });
    expect(showAfterFollow.status()).toBe(200);
    expect((await showAfterFollow.json()).isFollowing).toBe(true);

    // 9. timeline (= channel 内 note 一覧、empty で OK)
    const tlResp = await callApi(request, 'channels/timeline', {
      channelId,
      limit: 5,
    });
    expect(tlResp.status()).toBe(200);
    expect(Array.isArray(await tlResp.json())).toBe(true);

    // 10. favorite → my-favorites → unfavorite
    const favResp = await callApi(request, 'channels/favorite', {
      i: me.token,
      channelId,
    });
    expect(favResp.status()).toBe(204);

    const myFavResp = await callApi(request, 'channels/my-favorites', {
      i: me.token,
    });
    expect(myFavResp.status()).toBe(200);
    const myFavs = (await myFavResp.json()) as Channel[];
    expect(myFavs.find((c) => c.id === channelId)).toBeDefined();

    // show 再取得で isFavorited も反映されていること。
    const showAfterFav = await callApi(request, 'channels/show', {
      i: me.token,
      channelId,
    });
    expect(showAfterFav.status()).toBe(200);
    expect((await showAfterFav.json()).isFavorited).toBe(true);

    const unfavResp = await callApi(request, 'channels/unfavorite', {
      i: me.token,
      channelId,
    });
    expect(unfavResp.status()).toBe(204);

    // 11. mute/create → mute/list → mute/delete
    const muteCreateResp = await callApi(request, 'channels/mute/create', {
      i: me.token,
      channelId,
    });
    expect(muteCreateResp.status()).toBe(204);

    const muteListResp = await callApi(request, 'channels/mute/list', {
      i: me.token,
    });
    expect(muteListResp.status()).toBe(200);
    const muteList = (await muteListResp.json()) as Channel[];
    expect(muteList.find((c) => c.id === channelId)).toBeDefined();

    const muteDeleteResp = await callApi(request, 'channels/mute/delete', {
      i: me.token,
      channelId,
    });
    expect(muteDeleteResp.status()).toBe(204);

    // 12. unfollow → 204
    const unfollowResp = await callApi(request, 'channels/unfollow', {
      i: me.token,
      channelId,
    });
    expect(unfollowResp.status()).toBe(204);
  });

  test('channels/show returns negative for unknown channelId', async ({ request }) => {
    // mk-go: 404 NO_SUCH_CHANNEL
    // upstream Misskey TS: paramDef format: 'misskey:id' で pre-validation
    //   400 のケースがある (= reversi/show-game と同 pattern)。LCD で吸収。
    const resp = await callApi(request, 'channels/show', {
      channelId: '9zzzzzzzzzzzzzzz',
    });
    expect([400, 404]).toContain(resp.status());
  });
});
