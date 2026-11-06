/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 見た目の差分 (#4054): 未ログインの入口 (classic と simple)、サインインと
// 登録のダイアログ。入口の型は meta の clientOptions.entrancePageStyle を書き換えて
// 選ぶので、この spec は meta の project で流れる (playwright.config.ts)。撮った後に
// 元へ戻す。clientOptions は既存の値への浅いマージなので、この鍵だけを送ればよい。
//
// 入口に出る、他の spec のデータで変わるもの (classic の流れるタイムライン、
// 利用者数などの数字、連合しているサーバー) は、場所を保ったまま見えなくする。

import { readFileSync } from 'node:fs';
import { type APIRequestContext, expect, type Page, test } from '@playwright/test';
import { isTsBackend } from '../../../../fixtures/backend';
import { clickByTestId, clickWhenReady } from '../../../../fixtures/ui_click';
import { coversRoutes } from '../../../../fixtures/ui_route';
import { apiOk } from '../../../../fixtures/ui_timeline';
import { prepareVisualPage, SCHEMES, snap, visualWorld } from '../../../../fixtures/visual';

async function openEntrance(page: Page, baseURL: string): Promise<void> {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(`${baseURL}/`, { waitUntil: 'domcontentloaded' });
  await page.getByTestId('signin').first().waitFor();
}

// 入口で、他の spec のデータで変わる部分。クラス名は本番のビルドでハッシュに
// なるので testid で取る。ダイアログの背後に回るので、mask (一番上に描かれる) では
// なく、場所を保ったまま見えなくする
const ENTRANCE_VOLATILE = ['entrance-timeline', 'entrance-federation', 'visitor-stats', 'visitor-timeline', 'visitor-active-users'].map((id) => `[data-testid="${id}"]`);

test.describe('visual: entrance, signin and signup', () => {
  test.skip(isTsBackend, '見た目の差分は Elythia の frontend だけを撮る');
  test.setTimeout(90_000);

  let root: { token: string };
  let originalStyle: string;
  const setStyle = (request: APIRequestContext, style: string) =>
    apiOk(request, 'admin/update-meta', { i: root.token, clientOptions: { entrancePageStyle: style } });
  test.beforeAll(async ({ request }) => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    // 未設定なら classic (welcome.vue の既定)。clientOptions は浅いマージで鍵を消せないので、
    // 未設定だったときは明示の classic を書いて戻す (見え方は同じ)
    originalStyle = (await apiOk(request, 'admin/meta', { i: root.token })).clientOptions?.entrancePageStyle ?? 'classic';
  });
  test.afterAll(async ({ request }) => {
    await setStyle(request, originalStyle);
  });

  for (const scheme of SCHEMES) {
    for (const style of ['classic', 'simple'] as const) {
      test(`entrance (${style}, ${scheme})`, coversRoutes('/'), async ({ page, baseURL, request }) => {
        await setStyle(request, style);
        await prepareVisualPage(page, scheme);
        await openEntrance(page, baseURL!);
        // その型の入口になったことを確かめる (流れるタイムラインは classic にだけある)。
        // 確かめないと、型の切り替えが効かずに同じ絵を 2 枚撮っても通る
        await expect(page.getByTestId('entrance-timeline')).toHaveCount(style === 'classic' ? 1 : 0);
        await snap(page, `entrance-${style}-${scheme}`, { hide: ENTRANCE_VOLATILE });
      });
    }

    test(`signin dialog at the password step (${scheme})`, async ({ page, baseURL, request }) => {
      const w = await visualWorld(request, baseURL!);
      await setStyle(request, 'simple');
      await prepareVisualPage(page, scheme);
      await openEntrance(page, baseURL!);
      await clickByTestId(page, 'signin');
      await page.locator('[data-testid="signin-username"] input').fill(w.alice.username);
      await page.locator('[data-testid="signin-username"] input').press('Enter');
      await page.getByTestId('signin-page-password').waitFor();
      await snap(page, `signin-password-${scheme}`, { hide: ENTRANCE_VOLATILE });
    });

    test(`signup dialog: rules and the form (${scheme})`, async ({ page, baseURL, request }) => {
      await setStyle(request, 'simple');
      await prepareVisualPage(page, scheme);
      await openEntrance(page, baseURL!);
      await clickByTestId(page, 'signup');
      const cont = page.getByTestId('signup-rules-continue');
      await cont.waitFor();
      await snap(page, `signup-rules-${scheme}`, { hide: ENTRANCE_VOLATILE });
      // 同意のスイッチを押すと確認が出る。出ているものを全部押して先へ進む
      // (specs/upstream/ui/signup_dialog.spec.ts と同じ取り方)
      const agreeCount = await page.locator('input[type="checkbox"]').evaluateAll((inputs) =>
        inputs.filter((i) => (i.parentElement?.textContent ?? '').trim() === 'Agree').length);
      expect(agreeCount).toBeGreaterThanOrEqual(1);
      for (let n = 0; n < agreeCount; n++) {
        await clickWhenReady(page, `${n + 1} つ目の同意のスイッチ`, (idx: number) =>
          Array.from(document.querySelectorAll<HTMLInputElement>('input[type="checkbox"]'))
            .filter((i) => (i.parentElement?.textContent ?? '').trim() === 'Agree')[idx], n);
        await clickByTestId(page, 'modal-dialog-ok');
        await expect(page.getByTestId('modal-dialog-ok')).toHaveCount(0, { timeout: 10_000 });
      }
      await expect(cont).toBeEnabled({ timeout: 10_000 });
      await cont.click();
      await page.locator('[data-testid="signup-username"] input').waitFor();
      await snap(page, `signup-form-${scheme}`, { hide: ENTRANCE_VOLATILE });
    });
  }
});
