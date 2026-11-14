/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定の表 (#4056): /settings/preferences の「Other」の見出しの端末の設定を、
// 部品の操作 → 保存 → 再読み込みの後の表示 → 効き目、の順に確かめる。
//
// 効き目は、設定を変えていない (既定の値の) ときには必ず落ちる形で書く
// (`PW_SETTINGS_MUTATION=1` の変異の確かめ)。この見出しには時刻・端末・外部の
// 状態で効くものが多く、それらは理由を書いて効き目を省く。

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { pageBody } from '../../../../fixtures/route_check';
import { coversPrefs, expectHappens, type PrefRow, prefTitle, runPrefRow } from '../../../../fixtures/settings_table';
import { distinctPNG, uploadPNG } from '../../../../fixtures/ui_drive';
import { gotoRoute } from '../../../../fixtures/ui_route';
import { postNote, waitForApiBody } from '../../../../fixtures/ui_timeline';

const ROUTE = '/settings/preferences';

test.describe('settings: preferences (Other)', () => {
  test.setTimeout(60_000);
  // hemisphere の既定は時刻帯で決まる (utility/intl-const.ts。南半球の時刻帯なら S)。
  // 既定が目標の S と同じになると、行が何も確かめないので、時刻帯を固定する
  test.use({ timezoneId: 'UTC' });
  const ROWS: PrefRow[] = [
    {
      key: 'squareAvatars', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, baseURL }) => {
        await gotoRoute(page, baseURL, '/settings/profile');
        const radius = await page.locator('img[class*="_noSelect"], ._noSelect').first().evaluate((el) => getComputedStyle(el).borderRadius);
        expect(radius).toBe('20%');
      },
    },
    {
      // 本家 2026.11.0 で増えた (#4164)。既定はオンなので、切る向きで確かめる
      key: 'autoplayEmbededPlayerOfUrlPreview', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: { skip: 'URL プレビューの埋め込みのプレイヤー (YouTube など) を開いたときに、自動で再生するかを埋め込みの URL の引数で渡す (utility/url-preview.ts)。プレイヤーを持つ URL の要約は外部のサイトから取るので、隔離した環境では作れない' },
    },
    {
      key: 'enableSeasonalScreenEffect', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: { skip: '起動時に、月 (と hemisphere) で決まる季節だけ雪か桜の WebGL2 の canvas を足す (boot/main-boot.ts → utility/seasonal-screen-effect.ts)。時刻に依存する' },
    },
    {
      key: 'enableNightMeteorEffect', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: { skip: '夜の時刻・ダークテーマ・アニメーションありのときだけ、流れ星の canvas を足す (boot/main-boot.ts → utility/night-meteor-effect.ts)。時刻に依存する' },
    },
    {
      key: 'imageNewTab', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, request, baseURL, user }) => {
        const file = await uploadPNG(request, baseURL, user.token, 'pwnewtab.png', distinctPNG());
        const noteId = await postNote(request, user, null, { fileIds: [file.id] });
        await gotoRoute(page, baseURL, `/notes/${noteId}`);
        const media = pageBody(page).getByTestId('note-detailed').getByTestId('media-image');
        await expect(media).toBeVisible({ timeout: 20_000 });
        // 既定ではライトボックスで開く。新しいタブで開くと window.open が新しいページを作る (MkMediaList.vue)
        const opened = page.context().waitForEvent('page', { timeout: 10_000 });
        await media.click();
        await expectHappens(opened, '新しいタブで開かない');
        await expect(page.getByTestId('lightbox')).toHaveCount(0);
      },
    },
    {
      key: 'defaultFollowWithReplies', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, request, baseURL }) => {
        const other = await signupUser(request, randomUsername('pwfolr'));
        await gotoRoute(page, baseURL, `/@${other.username}`);
        const button = pageBody(page).locator('button').filter({ hasText: /^\s*Follow\s*$/ });
        await expect(button).toHaveCount(1, { timeout: 20_000 });
        const sent = waitForApiBody(page, 'following/create', () => true, 10_000);
        await button.click();
        // 既定ではフォローの前に確かめる (alwaysConfirmFollow)
        await page.getByTestId('modal-dialog-ok').click();
        expect((await sent).withReplies).toBe(true);
      },
    },
    {
      key: 'serverDisconnectedBehavior', route: ROUTE, control: { kind: 'select', label: 'Show warning dialog' }, value: 'dialog',
      effect: { skip: 'ストリーミングの接続が切れたときの振る舞い (boot/main-boot.ts、realtimeMode のときだけ)。切断を作るには WebSocket を差し替える仕組みが要る' },
    },
    {
      key: 'numberOfPageCache', route: ROUTE, control: { kind: 'range', value: 10, min: 1, max: 10 }, value: 10,
      effect: { skip: 'KeepAlive に残すページの数 (RouterView.vue の max)。何枚まで状態が残るかは、画面の上に出る差が無く観測しにくい' },
    },
    {
      key: 'forceShowAds', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: { skip: '広告を隠せる (canHideAds) 利用者にも広告を出す (MkAd.vue)。広告の登録とロールのポリシーはサーバー全体に効き、他の spec の画面 (見た目の差分) に広告が出るので、ここでは作らない' },
    },
    {
      key: 'hemisphere', route: ROUTE, control: { kind: 'radio', label: 'Southern Hemisphere' }, value: 'S',
      effect: { skip: '季節の演出 (enableSeasonalScreenEffect) の月の判定だけに使う (utility/seasonal-screen-effect.ts)。時刻に依存する' },
    },
  ];
  for (const row of ROWS) test(prefTitle(row), coversPrefs(row.key), ({ page, request, baseURL }) => runPrefRow({ page, request, baseURL }, row));
});
