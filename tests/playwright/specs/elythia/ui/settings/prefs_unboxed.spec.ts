/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 端末の設定 (#4056) のうち、部品が MkPreferenceContainer の外にあるもの (data-pref-key が
// 無いので文言で取る) と、ボタンやダイアログで値を決めるもの、前提の操作が要るもの。

import { expect, type Locator, type Page, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { pageBody } from '../../../../fixtures/route_check';
import { coversPrefs, labeledSwitch, openAllFolders, type PrefRow, prefTitle, runPrefRow } from '../../../../fixtures/settings_table';
import { distinctPNG, uploadPNG } from '../../../../fixtures/ui_drive';
import { clickDriveItem } from '../../../../fixtures/ui_drive';
import { clickMenuItem, clickOnlyTestId } from '../../../../fixtures/ui_post_form';
import { gotoRoute } from '../../../../fixtures/ui_route';
import { apiOk, postNote, runTag } from '../../../../fixtures/ui_timeline';

/** The MkSwitch root of the switch labeled `label` (setControl finds the toggle inside it). */
async function switchRoot(page: Page, label: string): Promise<Locator> {
  return (await labeledSwitch(page, label)).locator('..');
}

/** The innermost element that has both the text `label` and the options of a MkRadios. */
function radiosLabeled(page: Page, label: string): Locator {
  return pageBody(page).locator('div').filter({ has: page.getByText(label, { exact: true }) }).filter({ has: page.locator('[role="checkbox"]') }).last();
}

/** Opens the menu of the user's own note on its page and returns the menu items' texts. */
async function ownNoteMenu(page: Page, baseURL: string, noteId: string): Promise<void> {
  await gotoRoute(page, baseURL, `/notes/${noteId}`);
  await pageBody(page).getByTestId('note-detailed').getByTestId('note-menu').click();
}

test.describe('settings: preferences outside a preference container', () => {
  test.setTimeout(60_000);
  const ROWS: PrefRow[] = [
    {
      key: 'overridedDeviceKind', route: '/settings/preferences', control: { kind: 'radio', label: 'Smartphone' }, value: 'smartphone',
      container: async (page) => radiosLabeled(page, 'Device type'),
      effect: async ({ page }) => {
        // スマートフォンの UI では、左のナビバーの代わりに下のメニュー (ti-menu-2) が出る
        await expect(page.locator('button:has(i.ti-menu-2)').first()).toBeVisible();
        await expect(page.getByTestId('open-post-form').first()).toBeHidden();
      },
    },
    {
      key: 'pollingInterval', route: '/settings/preferences', value: 3,
      // リアルタイムモード (store の値) を切ると欄が操作できるようになる。切るのは前提 (setup) で、
      // 変異の確かめでは頻度を変える操作だけを省く (リアルタイムモードは切れたまま、既定の Medium)
      setup: async ({ page, baseURL }) => {
        await gotoRoute(page, baseURL, '/settings/preferences');
        await openAllFolders(page);
        // 状態を見てから押す (オフのときに押すと、オンに戻してしまう)
        const realtime = await labeledSwitch(page, 'Real-time mode');
        if ((await realtime.getAttribute('data-checked')) === 'true') await realtime.click();
        await expect(realtime).toHaveAttribute('data-checked', 'false');
      },
      control: { kind: 'range', value: 3, min: 1, max: 3 },
      effect: {
        skip: 'リアルタイムモードを切ったときの読み直しの間隔 (High 10 秒・Medium 15 秒。MkStreamingNotesTimeline.vue)。'
          + '時計を進めると、ストリーミングが応答の無いことを理由につなぎ直し、つなぎ直しの穴埋め (#3132) でも notes/timeline を読むので、'
          + '読んだ回数で間隔を見分けられない (#4056 で確かめた)',
      },
    },
    {
      key: 'skipNoteRender', route: '/settings/other', control: { kind: 'switch', on: false }, value: false,
      container: (page) => switchRoot(page, 'Enable note render skipping'),
      effect: async ({ page, request, baseURL, user }) => {
        const text = runTag('pwskip');
        await postNote(request, user, text);
        await gotoRoute(page, baseURL, `/@${user.username}/notes`);
        const article = pageBody(page).locator('article').filter({ hasText: text });
        await expect(article).toHaveCount(1, { timeout: 20_000 });
        // 描画の省略を切ると、ノートの content-visibility が auto から visible になる。付くのは
        // MkNote のいちばん外の要素 (tabindex="0" の div。MkNote.vue の skipRender)
        await expect.poll(() => article.evaluate((el) => {
          const root = el.closest('div[tabindex="0"]');
          return root ? getComputedStyle(root).contentVisibility : 'no MkNote root';
        })).toBe('visible');
      },
    },
    {
      key: 'experimental.stackingRouterView', route: '/settings/other', control: { kind: 'switch', on: true }, value: true,
      container: (page) => switchRoot(page, 'Enable stacking router view'),
      effect: async ({ page }) => {
        // StackingRouterView では、ページの本体が --i の style を持つタブの中に入る (StackingRouterView.vue)
        await expect.poll(() => pageBody(page).evaluate((el) => el.closest('[style*="--i"]') != null)).toBe(true);
      },
    },
    {
      key: 'experimental.enableFolderPageView', route: '/settings/other', control: { kind: 'switch', on: true }, value: true,
      container: (page) => switchRoot(page, 'Enable folder page view'),
      effect: async ({ page, baseURL }) => {
        // スマートフォンのときだけ、フォルダが別のページとして開く印 (ti-chevron-right) を出す (MkFolder.vue)
        await page.evaluate(() => {
          const profile = JSON.parse(window.localStorage.getItem('preferences') ?? '{}');
          profile.preferences.overridedDeviceKind = [[{}, 'smartphone', {}]];
          window.localStorage.setItem('preferences', JSON.stringify(profile));
        });
        await gotoRoute(page, baseURL, '/settings/preferences');
        await page.reload({ waitUntil: 'domcontentloaded' });
        await expect(pageBody(page).locator('[data-testid="folder-header"] i.ti-chevron-right').first()).toBeVisible({ timeout: 20_000 });
      },
    },
    {
      key: 'experimental.enableHapticFeedback', route: '/settings/other', control: { kind: 'switch', on: true }, value: true,
      container: (page) => switchRoot(page, 'Enable haptic feedback'),
      effect: { skip: '触覚 (utility/haptic.ts の ios-haptics) は、ブラウザの中で起きたかを確かめられない' },
    },
    {
      key: 'experimental.enableWebTranslatorApi', route: '/settings/other', control: { kind: 'switch', on: true }, value: true,
      container: (page) => switchRoot(page, 'Enable in-browser translator API'),
      effect: { skip: 'ブラウザの Translator API がある環境でだけ、ノートのメニューに「Translate」が出る (get-note-menu.ts)。headless の Chromium には無い' },
    },
    {
      key: 'devMode', route: '/settings/other', control: { kind: 'switch', on: true }, value: true,
      container: (page) => switchRoot(page, 'Developer mode'),
      effect: async ({ page, request, baseURL, user }) => {
        // 開発者モードでは、ノートのメニューに「Copy note ID」が出る (get-note-menu.ts)
        await ownNoteMenu(page, baseURL, await postNote(request, user, runTag('pwdev')));
        await expect(page.getByText('Copy note ID', { exact: true })).toBeVisible();
      },
    },
    {
      key: 'syncDeviceDarkMode', route: '/settings/theme', control: { kind: 'switch', on: false }, value: false,
      container: (page) => switchRoot(page, 'Sync Dark Mode with your device settings'),
      effect: async ({ page }) => {
        // 追従を切ると、OS をダークにしても <html> はライトのまま
        await page.emulateMedia({ colorScheme: 'light' });
        await expect.poll(() => page.evaluate(() => document.documentElement.dataset.colorScheme)).toBe('light');
        await page.emulateMedia({ colorScheme: 'dark' });
        // 変わらないことは待てないので、1 秒の間 100ms ごとに見る
        for (let i = 0; i < 10; i++) {
          expect(await page.evaluate(() => document.documentElement.dataset.colorScheme)).toBe('light');
          await page.waitForTimeout(100);
        }
      },
    },
    {
      key: 'showSoftWordMutedWord', route: '/settings/mute-block', control: { kind: 'switch', on: true }, value: true,
      container: (page) => switchRoot(page, 'Show muted words'),
      setup: async ({ request, user }) => {
        const word = runTag('pwmute');
        await apiOk(request, 'i/update', { i: user.token, mutedWords: [[word]] });
        const other = await signupUser(request, randomUsername('pwmuted'));
        await postNote(request, other, `about ${word} today`);
        return { word, other };
      },
      effect: async ({ page, baseURL, data }) => {
        const { word, other } = data as { word: string; other: { username: string } };
        // ミュートした語に当たったノートは畳まれ、どの語に当たったかが出る (MkNote.vue)
        await gotoRoute(page, baseURL, `/@${other.username}/notes`);
        await expect(pageBody(page).getByText(`"${word}"`).first()).toBeVisible({ timeout: 20_000 });
      },
    },
    {
      key: 'mutingEmojis', route: '/settings/mute-block', value: ['🍮'],
      control: {
        kind: 'custom',
        set: async ({ page }) => {
          const folder = pageBody(page).locator('[role="group"]').filter({ has: page.getByText('Mute emoji', { exact: true }) }).last();
          await folder.getByRole('button', { name: 'Add' }).click();
          // このピッカーは固定の絵文字の欄を出さない (showPinned: false) ので、検索して選ぶ
          await page.getByTestId('emoji-picker-search').fill('custard');
          await page.getByTestId('emoji-picker-search-result').first().click();
        },
        shows: async ({ page }) => {
          const folder = pageBody(page).locator('[role="group"]').filter({ has: page.getByText('Mute emoji', { exact: true }) }).last();
          await expect(folder.getByText('🍮').or(folder.locator('img[alt="🍮"]')).first()).toBeVisible();
        },
      },
      effect: async ({ page, request, baseURL, user }) => {
        // ミュートした絵文字は、ノートの本文で unknown の画像に置き換わる (MkEmoji.vue)。
        // 設定のページの見本はミュートを無視して描くので、ノートで見る
        await gotoRoute(page, baseURL, `/notes/${await postNote(request, user, `${runTag('pwemoji')} 🍮`)}`);
        const note = pageBody(page).getByTestId('note-detailed');
        // unknown.png は本番のビルドでハッシュ付きの名前 (/vite/assets/…) になる。ミュートしていなければ /twemoji/ から読む
        const img = note.locator('img[alt="🍮"]');
        await expect(img).toHaveAttribute('src', /\/vite\/assets\/.+\.png$/, { timeout: 20_000 });
      },
    },
    {
      key: 'deck.wallpaper', route: '/settings/deck',
      setup: async ({ request, baseURL, user }) => uploadPNG(request, baseURL, user.token, 'pw-wallpaper.png', distinctPNG()),
      value: (file: unknown) => (file as { url: string }).url,
      control: {
        kind: 'custom',
        set: async ({ page }, file) => {
          await pageBody(page).getByRole('button', { name: 'Set wallpaper' }).click();
          await clickMenuItem(page, 'From Drive');
          await clickDriveItem(page, 'drive-file', (file as { name: string }).name);
          await clickOnlyTestId(page, 'modal-window-ok');
        },
        shows: async ({ page }) => {
          await expect(pageBody(page).getByRole('button', { name: 'Remove wallpaper' })).toBeVisible();
        },
      },
      effect: async ({ page, baseURL, data }) => {
        // デッキの本体の背景に、その画像が敷かれる (ui/deck.vue)
        await page.evaluate(() => window.localStorage.setItem('ui', 'deck'));
        await gotoRoute(page, baseURL, '/');
        await page.reload({ waitUntil: 'domcontentloaded' });
        const url = (data as { url: string }).url;
        await expect.poll(() => page.evaluate((u) => Array.from(document.querySelectorAll<HTMLElement>('[style*="background-image"]')).some((el) => el.style.backgroundImage.includes(u)), url), { timeout: 20_000 }).toBe(true);
      },
    },
  ];
  for (const row of ROWS) test(prefTitle(row), coversPrefs(row.key), ({ page, request, baseURL }) => runPrefRow({ page, request, baseURL }, row));
});
