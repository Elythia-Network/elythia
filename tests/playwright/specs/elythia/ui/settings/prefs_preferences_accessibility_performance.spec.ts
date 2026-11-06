/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定の表 (#4056): /settings/preferences の「Accessibility」「Performance」の
// 見出しの端末の設定を、部品の操作 → 保存 → 再読み込みの後の表示 → 効き目、の
// 順に確かめる。
//
// 効き目は、設定を変えていない (既定の値の) ときには必ず落ちる形で書く
// (`PW_SETTINGS_MUTATION=1` の変異の確かめ)。「無い」ことを見るときは、本来
// それがある画面にいることを先に確かめる。
//
// animation と disableShowingAnimatedImages は両方の見出しにスイッチがあり、
// 片方は値を反転して出す (preferences.vue)。1 キー 1 行なので、反転するほうを
// 操作する: animation は Accessibility の「Reduce UI animations」(nth 0)、
// disableShowingAnimatedImages は Performance の「Enable animated images」(nth 1)。
//
// animation と animatedMfm の既定は OS の「動きを減らす」に従う。Playwright の
// 既定 (no-preference) では、どちらも既定が true になる。

import { expect, test } from '@playwright/test';
import { pageBody } from '../../../../fixtures/route_check';
import { coversPrefs, prefContainer, type PrefRow, prefTitle, runPrefRow } from '../../../../fixtures/settings_table';
import { distinctPNG, uploadPNG } from '../../../../fixtures/ui_drive';
import { gotoRoute } from '../../../../fixtures/ui_route';
import { apiOk, postNote, runTag } from '../../../../fixtures/ui_timeline';

const ROUTE = '/settings/preferences';

test.describe('settings: preferences (Accessibility, Performance)', () => {
  test.setTimeout(60_000);
  const ROWS: PrefRow[] = [
    // Accessibility
    {
      key: 'animation', route: ROUTE, nth: 0, control: { kind: 'switch', on: true }, value: false,
      effect: async ({ page }) => {
        // boot/common.ts が、アニメーションを切ると <html> に素のクラス _noAnimation_ を付ける
        await expect(page.locator('html')).toHaveClass(/(^|\s)_noAnimation_(\s|$)/);
      },
    },
    {
      key: 'disableShowingAnimatedImages', route: ROUTE, nth: 1, control: { kind: 'switch', on: false }, value: true,
      effect: async ({ page, request, baseURL, user }) => {
        // 既定のアバター (同一オリジンの /identicon/) は静止画の URL に変えないので、画像を上げてアバターにする
        const file = await uploadPNG(request, baseURL, user.token, 'pwavatar.png', distinctPNG());
        await apiOk(request, 'i/update', { i: user.token, avatarId: file.id });
        await gotoRoute(page, baseURL, `/@${user.username}`);
        const img = pageBody(page).locator(`._noSelect[title="${user.username}"] img`).first();
        await expect(img).toBeAttached({ timeout: 20_000 });
        // 静止画はメディアプロキシの static=1 を通す (media-proxy.ts の getStaticImageUrl)
        await expect(img).toHaveAttribute('src', /[?&]static=1(&|$)/);
      },
    },
    {
      key: 'animatedMfm', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page, request, baseURL, user }) => {
        const word = runTag('pwspin');
        await gotoRoute(page, baseURL, `/notes/${await postNote(request, user, `$[spin ${word}]`)}`);
        const note = pageBody(page).getByTestId('note-detailed');
        await expect(note.getByText(word)).toBeVisible({ timeout: 20_000 });
        // 動く MFM は、要素の style に animation を書いて動かす (mfm-renderer.ts)
        await expect(note.locator('[style*="animation"]')).toHaveCount(0);
      },
    },
    {
      key: 'showPageTabBarBottom', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, baseURL }) => {
        await gotoRoute(page, baseURL, '/explore');
        const tab = pageBody(page).locator('button').filter({ hasText: /^\s*Roles\s*$/ });
        await expect(tab).toHaveCount(1);
        const top = (await pageBody(page).boundingBox())!.y;
        // 既定ではタブは上のヘッダー (高さ 50px ほど) にある。下に置くと本文の後ろのフッターに出る
        await expect.poll(async () => ((await tab.boundingBox())?.y ?? 0) - top, { message: 'タブが下に出ない' }).toBeGreaterThan(100);
      },
    },
    {
      key: 'enableHorizontalSwipe', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: { skip: 'タッチのスワイプでタブを切り替える (PageWithHeader.vue → MkSwiper.vue)。DOM の差はハッシュのクラスだけで、マウスでは操作できない' },
    },
    {
      key: 'enablePullToRefresh', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: { skip: 'タッチで一覧を引き下げて読み直す (MkPagination.vue などの MkPullToRefresh)。マウスでは操作できない' },
    },
    {
      key: 'keepScreenOn', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: { skip: '起動時に navigator.wakeLock.request("screen") を呼ぶ (boot/common.ts)。画面が消えるかどうかは端末に依存する' },
    },
    {
      key: 'useNativeUiForVideoAudioPlayer', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: { skip: 'ライトボックスで動画と音声にブラウザの再生 UI を使う (MkLightbox.item.vue)。ブラウザで再生できる動画か音声 (ogg・mp3・mp4 など) のファイルが要り、fixture にまだ無い' },
    },
    {
      key: 'makeEveryTextElementsSelectable', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page }) => {
        // 起動時に <html> に素のクラス forceSelectableAll を付ける (boot/common.ts)。既定はデスクトップで true
        await expect(page.locator('html')).not.toHaveClass(/(^|\s)forceSelectableAll(\s|$)/);
      },
    },
    {
      key: 'menuStyle', route: ROUTE, control: { kind: 'select', label: 'Drawer' }, value: 'drawer',
      effect: async ({ page }) => {
        // 設定の行の「...」でメニューを開く。ポップアップは背景を透かし、ドロワーは透かさない (MkModal.vue)
        await prefContainer(page, 'menuStyle').locator('button:has(> i.ti-dots)').click();
        await expect(page.locator('[role="menu"]')).toBeVisible();
        await expect(page.getByTestId('bg').last()).toHaveAttribute('data-test-is-transparent', 'false');
      },
    },
    {
      key: 'contextMenu', route: ROUTE, control: { kind: 'select', label: 'Application with shift key' }, value: 'appWithShift',
      effect: async ({ page }) => {
        // 設定の行は右クリックでアプリのメニューを出す (MkPreferenceContainer.vue)。選択の部品は mousedown で
        // 開くので、右クリックは「...」のボタンに当てる
        const target = prefContainer(page, 'contextMenu').locator('button:has(> i.ti-dots)');
        await target.scrollIntoViewIfNeeded();
        await target.click({ button: 'right' });
        await page.waitForTimeout(500);
        await expect(page.locator('[role="menu"]')).toHaveCount(0);
        await target.click({ button: 'right', modifiers: ['Shift'] });
        await expect(page.locator('[role="menu"]')).toBeVisible();
      },
    },
    // Performance
    {
      key: 'useBlurEffect', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page }) => {
        // 切ると <html> の style に --MI-blur: none を書く。既定では書かない (boot/common.ts)
        await expect.poll(() => page.evaluate(() => document.documentElement.style.getPropertyValue('--MI-blur').trim())).toBe('none');
      },
    },
    {
      key: 'useBlurEffectForModal', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page }) => {
        // 既定では <html> の style の --MI-modalBgFilter が blur(4px) (boot/common.ts)
        await expect.poll(() => page.evaluate(() => document.documentElement.style.getPropertyValue('--MI-modalBgFilter').trim())).toBe('none');
      },
    },
    {
      key: 'enableHighQualityImagePlaceholders', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page, baseURL, user }) => {
        await gotoRoute(page, baseURL, `/@${user.username}`);
        const avatar = pageBody(page).locator(`._noSelect[title="${user.username}"]`).first();
        await expect(avatar).toBeVisible({ timeout: 20_000 });
        // 既定では、アバターの画像を blurhash の部品 (素のクラス chromatic-ignore) で包む (MkAvatar.vue)
        await expect(pageBody(page).locator(`._noSelect[title="${user.username}"] .chromatic-ignore`)).toHaveCount(0);
      },
    },
    {
      key: 'useStickyIcons', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page, request, baseURL, user }) => {
        const text = runTag('pwsticky');
        await postNote(request, user, text);
        // プロフィールの概要のタブには固定のノートしか出ないので、ノートのタブで探す
        await gotoRoute(page, baseURL, `/@${user.username}/notes`);
        const article = pageBody(page).locator('article').filter({ hasText: text });
        await expect(article).toHaveCount(1, { timeout: 20_000 });
        // 既定では、ノートのアバターがスクロールに付いてくる (MkNote.vue の position: sticky)
        await expect.poll(() => article.locator('._noSelect').first().evaluate((el) => getComputedStyle(el).position)).not.toBe('sticky');
      },
    },
  ];
  for (const row of ROWS) test(prefTitle(row), coversPrefs(row.key), ({ page, request, baseURL }) => runPrefRow({ page, request, baseURL }, row));
});
