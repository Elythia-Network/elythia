/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/avatar-decoration で 装着済 decoration を click → XDialog →
// "Detach" button (ti-x + "Detach") click → /api/i/update が
// avatarDecorations 配列を縮めて round-trip する write-flow spec。
//
// avatar-decoration.dialog.vue:40 の Detach button は usingIndex != null
// (= 装着済) のとき表示される。click すると親の openDecoration callback
// "detach" 経由で i/update に新 avatarDecorations 配列 (= 該当 entry を
// 除いた配列) を送る。
//
// avatar_decoration_attach_via_dialog の sister。setup で必ず 1 装着の
// state にしておき、UI 経由で外して空配列に戻る path を verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /settings/avatar-decoration detach via dialog flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(90_000);

  test('signupUser-equivalent decoration setup → click attached → Detach → /api/i/update with empty avatarDecorations', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. admin/avatar-decorations/create で decoration を新規作成
    const decorationName = `pw-deco-d-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'admin/avatar-decorations/create', {
      i: root.token,
      name: decorationName,
      description: '',
      url: `https://example.invalid/decoration/${decorationName}.png`,
      roleIdsThatCanBeUsedThisDecoration: [],
    });
    expect(createResp.status()).toBeLessThan(400);
    const createdBody = await createResp.json();
    const decorationId: string = createdBody.id;
    const decorationUrl: string = createdBody.url;
    expect(decorationId).toBeTruthy();

    // 2. root の avatarDecorations を [新 decoration 1 個] に reset
    await callApi(request, 'i/update', {
      i: root.token,
      avatarDecorations: [
        {
          id: decorationId,
          angle: 0,
          flipH: false,
          offsetX: 0,
          offsetY: 0,
        },
      ],
    });

    // 3. /settings/avatar-decoration を開く
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/avatar-decoration`, {
      waitUntil: 'domcontentloaded',
    });

    // 装着 thumbnail (= openAttachedDecoration trigger) hydrate を待つ。
    // 装着済 decoration は <img src="<url>"> として render されるので
    // url を持つ img を待つ。
    //
    // **生 URL と media proxy 経由の両方を許す。** upstream TS は
    // `UserEntityService` が `avatarDecorations[].url` を raw で返すが、backend に
    // よっては media proxy 経由へ書き換える。その場合、元 URL は `?url=` へ
    // **percent-encode されて**入るので `src.includes(url)` では一致しない
    // (実測: `https://example.invalid/x.png` →
    // `/image.webp?sig=...&url=https%3A%2F%2Fexample.invalid%2Fx.png`)。
    // この spec は `specs/upstream/` にあり `make playwright-ts-test` が TS backend
    // に対しても回すので、**どちらか一方に決め打つと必ず片方で落ちる**。
    await page.waitForFunction(
      (u) => {
        const imgs = Array.from(document.querySelectorAll('img')) as HTMLImageElement[];
        return imgs.some((i) => {
          if (i.src.includes(u)) return true;
          try {
            return new URL(i.src, document.baseURI).searchParams.get('url') === u;
          } catch {
            return false;
          }
        });
      },
      decorationUrl,
      { timeout: 20_000 },
    );

    // 4. 装着済 thumbnail (= 上部 attached list の card) を click。
    // avatar-decoration.decoration.vue:7-9 の root は
    // `<div :class="$style.root" @click="emit('click')">`。Vue の @click は
    // addEventListener-based なので element.onclick property には現れない
    // (= 旧 spec の `node.onclick` 判定は常に false で fallback img.click()
    // に到達 → bubbling は通るが listener が走らない race の場面があり
    // 90s timeout していた)。
    //
    // production build では CSS module 名がハッシュ (`xndfW` 等) に潰れるため
    // `[class*="decorations"]` / `[class*="root"]` は 1 件も match せず、
    // click が誰にも届かないまま 90s timeout していた。
    //
    // avatar-decoration.vue は **上部 = 装着済 (attached) / 下部 = 利用可能
    // (available)** の順で card を並べ、本 spec の setup では同じ decoration
    // が両方に出る。decoration name を含む最内要素を DOM 順に取り、先頭
    // (= attached 側) を click する。HTMLElement.click() は bubbling する
    // click event を投げるので card root の @click まで届く。
    //
    // attached grid が available grid より後に mount する race があるため、
    // 「name を含む最内要素が 2 個 (= attached + available) 出揃う」まで待って
    // から先頭を click する。1 個の時点で click すると available 側を掴んで
    // Attach dialog が開き、Remove button が永久に現れない。
    await page.waitForFunction(
      (n) => {
        const hits = Array.from(document.querySelectorAll('*')).filter((el) =>
          (el.textContent ?? '').includes(n),
        );
        return (
          hits.filter((el) => !hits.some((other) => other !== el && el.contains(other)))
            .length >= 2
        );
      },
      decorationName,
      { timeout: 20_000 },
    );
    await clickWhenReady(page, 'decoration 名を含む最内要素', (n) => {
      const hits = Array.from(document.querySelectorAll('*')).filter((el) =>
        (el.textContent ?? '').includes(n),
      );
      const innermost = hits.filter(
        (el) => !hits.some((other) => other !== el && el.contains(other)),
      );
      return (innermost[0] as HTMLElement | undefined);
    }, decorationName);

    // 5. XDialog の detach button hydrate を待つ。
    // attach button は usingIndex == null のときに表示されるが、本 spec は
    // usingIndex != null path なので detach button が出現する。
    //
    // label は `i18n.ts.detach` で、en-US では **"Remove"** ("Detach" では
    // ない)。旧実装は "detach" を探していて永久に見つからなかった。
    // 装着一覧の "Remove All" とは ti-x icon の有無で区別する。
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some(
          (b) =>
            !b.disabled &&
            b.querySelector('i.ti-x') !== null &&
            (b.textContent ?? '').trim() === 'Remove',
        );
      },
      { timeout: 15_000 },
    );

    // 6. Detach click → i/update round-trip (avatarDecorations 配列が縮む)
    const updateResp = page.waitForResponse(
      // 無関係な i/update を掴まないよう payload で絞る (attach spec と同様)。
      (r) =>
        r.url().includes('/api/i/update') &&
        r.status() < 300 &&
        (r.request().postData() ?? '').includes('avatarDecorations'),
      { timeout: 15_000 },
    );
    await clickWhenReady(page, '「Remove」のボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) =>
          !b.disabled &&
          b.querySelector('i.ti-x') !== null &&
          (b.textContent ?? '').trim() === 'Remove',
      ),
    );
    const update = await updateResp;
    const body = await update.json();
    expect(Array.isArray(body.avatarDecorations)).toBe(true);
    expect(body.avatarDecorations.length).toBe(0);
  });
});
