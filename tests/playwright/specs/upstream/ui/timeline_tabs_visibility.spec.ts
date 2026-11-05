/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// `/` のタブを home / local / social / global と切り替え、それぞれで出るノートと
// 出ないノートを、公開範囲とフォロー関係ごとに確かめる (#4046)。
//
// 見る人 (viewer) は followee をフォローし、stranger はフォローしない。どの
// ノートがどのタブに出るかは本家のタイムラインの定義どおり:
//   - home: 自分とフォローしている人のノート (followers も、自分宛ての specified も)
//   - local: ローカルの public だけ
//   - social: home と local を合わせたもの
//   - global: public だけ (このスタックにリモートは無いので、local と同じ顔ぶれ)
// 判定はページの本体に絞り、出るものが揃ってから出ないものが無いことを見る。

import { expect, test } from '@playwright/test';
import { pollForTimelineNote } from '../../../fixtures/timeline';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { coversRoutes, expectRoutedPage } from '../../../fixtures/ui_route';
import { clickPageTab, expectNotesInBody, follow, freshUser, postNote, runTag, waitForApiBody } from '../../../fixtures/ui_timeline';

test.describe('UI: timeline tabs show the notes each timeline should have', () => {
  test.setTimeout(120_000);

  test('home / local / social / global follow visibility and following', coversRoutes('/'), async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwtlv');
    const followee = await freshUser(request, 'pwtlf');
    const stranger = await freshUser(request, 'pwtls');
    await follow(request, viewer, followee);

    const tag = runTag('tl');
    const t = {
      fPublic: `${tag}-followee-public`,
      fHome: `${tag}-followee-home`,
      fFollowers: `${tag}-followee-followers`,
      fToViewer: `${tag}-followee-specified-viewer`,
      fToStranger: `${tag}-followee-specified-stranger`,
      sPublic: `${tag}-stranger-public`,
      sHome: `${tag}-stranger-home`,
      sFollowers: `${tag}-stranger-followers`,
      vHome: `${tag}-viewer-home`,
    };
    await postNote(request, followee, t.fPublic, { visibility: 'public' });
    await postNote(request, followee, t.fHome, { visibility: 'home' });
    await postNote(request, followee, t.fFollowers, { visibility: 'followers' });
    await postNote(request, followee, t.fToViewer, { visibility: 'specified', visibleUserIds: [viewer.id] });
    await postNote(request, followee, t.fToStranger, { visibility: 'specified', visibleUserIds: [stranger.id] });
    const lastPublic = await postNote(request, stranger, t.sPublic, { visibility: 'public' });
    await postNote(request, stranger, t.sHome, { visibility: 'home' });
    await postNote(request, stranger, t.sFollowers, { visibility: 'followers' });
    const lastHome = await postNote(request, viewer, t.vHome, { visibility: 'home' });

    // タイムラインへの振り分けは非同期なので、画面を開く前に API で反映を待つ
    await pollForTimelineNote(request, 'notes/timeline', viewer.token, lastHome);
    await pollForTimelineNote(request, 'notes/local-timeline', viewer.token, lastPublic);

    const all = Object.values(t);
    const expectTab = async (shown: string[]) => {
      await expectNotesInBody(page, shown, all.filter((x) => !shown.includes(x)));
    };

    // サインインすると `/` の既定のタブ (home) が開く
    const homeSent = waitForApiBody(page, 'notes/timeline');
    await uiSigninAs(page, request, baseURL!, viewer);
    await expectRoutedPage(page, '/');
    await homeSent;
    await expectTab([t.fPublic, t.fHome, t.fFollowers, t.fToViewer, t.vHome]);

    const localSent = waitForApiBody(page, 'notes/local-timeline');
    await clickPageTab(page, 'timeline-tab-local');
    await localSent;
    await expectTab([t.fPublic, t.sPublic]);

    const socialSent = waitForApiBody(page, 'notes/hybrid-timeline');
    await clickPageTab(page, 'timeline-tab-social');
    await socialSent;
    await expectTab([t.fPublic, t.fHome, t.fFollowers, t.fToViewer, t.sPublic, t.vHome]);

    const globalSent = waitForApiBody(page, 'notes/global-timeline');
    await clickPageTab(page, 'timeline-tab-global');
    await globalSent;
    await expectTab([t.fPublic, t.sPublic]);

    // 戻すと home の顔ぶれに戻る (タブの切り替えが一覧を作り直している)
    const homeAgain = waitForApiBody(page, 'notes/timeline');
    await clickPageTab(page, 'timeline-tab-home');
    expect(await homeAgain).toMatchObject({ withRenotes: true });
    await expectTab([t.fPublic, t.fHome, t.fFollowers, t.fToViewer, t.vHome]);
  });
});
