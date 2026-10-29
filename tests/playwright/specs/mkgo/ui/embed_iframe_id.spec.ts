/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 埋め込みページのiframeIdが、埋め込み元の親ウィンドウの送った値で決まることを
// 実ブラウザで見る。iframeIdは最初の1回で確定するので、親以外の値を受けると
// 親への通知に別の値が付く。
//
// **`mkgo/`に置く。** 本家のfrontend-embedは送り元を確かめないので、公式imageを
// 使う`playwright-ts-test`では通らない。

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { createNote } from '../../../fixtures/notes';
import { resetRateLimit } from '../../../fixtures/rate_limit';

const REGISTER = 'misskey:embedParent:registerIframeId';

type HeightLog = { __heights: Array<string | null> };

test.describe('embed: iframeId registration', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('ignores an iframeId sent by a window other than the parent', async ({ request, page, baseURL }) => {
    const me = await signupUser(request, randomUsername('embid'));
    // 幅を変えたときに折り返しで高さが変わるよう、長い本文にする。
    const text = `embed-iframe-id-${Date.now()} ${'word '.repeat(80)}`;
    const note = await createNote(request, me.token, { text, visibility: 'public' });

    await page.setContent(
      `<iframe id="mk" src="${baseURL}/embed/notes/${note.id}" width="500" height="400"></iframe>`,
      { waitUntil: 'domcontentloaded' },
    );
    await page.evaluate(() => {
      const w = window as unknown as HeightLog;
      w.__heights = [];
      window.addEventListener('message', (event) => {
        if (event.data?.type === 'misskey:embed:changeHeight') {
          w.__heights.push(event.data.iframeId ?? null);
        }
      });
    });

    const frame = page.frameLocator('#mk');
    await expect(frame.getByText(/embed-iframe-id-/)).toBeVisible({ timeout: 30_000 });
    const embed = page.frame({ url: /\/embed\/notes\// });
    expect(embed).not.toBeNull();

    // 親ではないウィンドウ(ここでは埋め込みページ自身)が先に送る。同じウィンドウの
    // messageのタスクは、後に積んだタイマーより先に処理される。
    await embed!.evaluate((type) => {
      window.postMessage({ type, payload: { iframeId: 'not-from-parent' } }, '*');
    }, REGISTER);
    await embed!.evaluate(() => new Promise((resolve) => setTimeout(resolve, 200)));

    // 続いて親が送る。
    await page.evaluate((type) => {
      const el = document.getElementById('mk') as HTMLIFrameElement;
      el.contentWindow!.postMessage({ type, payload: { iframeId: 'from-parent' } }, '*');
    }, REGISTER);
    await page.evaluate(() => new Promise((resolve) => setTimeout(resolve, 200)));

    // 幅を狭めて高さを変え、親への通知に付くiframeIdを見る。親の送った値が埋め込み
    // ページに届く前の通知はnullになるので、iframeIdの付いた通知が来るまで待つ。
    await page.evaluate(() => {
      (window as unknown as HeightLog).__heights = [];
      (document.getElementById('mk') as HTMLIFrameElement).width = '220';
    });
    await expect
      .poll(() => page.evaluate(() => (window as unknown as HeightLog).__heights.filter((id) => id != null)), {
        timeout: 15_000,
      })
      .not.toEqual([]);
    const ids = await page.evaluate(() => (window as unknown as HeightLog).__heights.filter((id) => id != null));
    expect(ids).toEqual(ids.map(() => 'from-parent'));
  });
});
