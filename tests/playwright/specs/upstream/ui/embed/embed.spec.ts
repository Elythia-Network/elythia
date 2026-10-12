/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #2389: 埋め込み (embed) ページを実 browser で検証する。
//
// embed は mk-go が長らく未実装で、`/embed/notes/<id>` は SPA catchall に
// 落ちて **通常の Misskey アプリ**が返っていた。エラーにならないので、
// iframe に埋め込むと「動いているが中身が別物」という気付きにくい壊れ方を
// していた。HTTP status だけを見る検査ではこの状態を検出できない。
//
// そのため本 spec は 2 層で見る。
//
//   1. 初期 HTML の markup — embed 専用シェルであること (SPA シェルではない)
//   2. 実 browser の iframe — 実際に描画され、投稿本文が読めること
//
// 特に 2 が重要で、bundle の path や CSP・X-Frame-Options が食い違っていると
// HTML は 200 でも iframe 内が空になる。これは実 browser でしか分からない。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { isTsBackend } from '../../../../fixtures/backend';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('embed: 埋め込みページ', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('埋め込み用のシェルと bundle が配信される', async ({ request, baseURL }) => {
    // 埋め込み先サイトが読み込むローダー。
    const loader = await request.get(`${baseURL}/embed.js`);
    expect(loader.status()).toBe(200);

    // embed 専用 bundle。通常 SPA の /vite/ とは別 build なので、ここが
    // 404 だと iframe 内が真っ白になる。
    const manifest = await request.get(`${baseURL}/embed_vite/manifest.json`);
    expect(manifest.status()).toBe(200);

    const resp = await request.get(`${baseURL}/embed/notes/nonexistent`);
    expect(resp.status()).toBe(200);
    const html = await resp.text();

    if (isTsBackend) {
      // upstream の `/embed/notes/:note` は対象が無い / 埋め込み不可のとき
      // **何も返さず** fastify の既定 (空 body) に落ちる
      // (`ClientServerService.ts` の `if (note == null) return;`)。
      expect(html).toBe('');
      return;
    }

    // mk-go は同じ条件でも**文脈なしのシェル**を返す。「ノートが無い」と
    // 「非公開」を応答の形で区別させないための意図的な差分で、可視性ゲート
    // 自体は upstream と同一 (`internal/server/embed.go`)。
    //
    // embed シェルであることの指標。通常シェルは /vite/ を読み #splash を
    // 持つので、取り違えるとここで落ちる。
    expect(html).toContain('/embed_vite/');
    expect(html).toContain('name="robots" content="noindex"');
    expect(html).not.toContain('<div id="splash">');
  });

  test('埋め込みページは iframe 内で描画され本文が読める', async ({
    request,
    page,
    baseURL,
  }) => {
    const me = await signupUser(request, randomUsername('emb'));
    const text = `embed-render-${Date.now()}`;
    const note = await createNote(request, me.token, { text, visibility: 'public' });

    // 実際の利用形態に寄せて iframe 経由で開く。直接 goto すると
    // X-Frame-Options や frame 内での bundle 解決の問題を素通りしてしまう。
    await page.setContent(
      `<iframe id="mk" src="${baseURL}/embed/notes/${note.id}" width="600" height="400"></iframe>`,
      { waitUntil: 'domcontentloaded' },
    );

    const frame = page.frameLocator('#mk');
    // Vue の mount 完了 = 投稿本文が DOM に出ること。bundle が読めていない
    // 場合はここで timeout する (HTML 自体は 200 で返っているので、status
    // だけ見る検査では捕まらない)。
    await expect(frame.getByText(text)).toBeVisible({ timeout: 30_000 });
  });

  test('X-Frame-Options が embed には付かず通常ページには付く', async ({
    request,
    baseURL,
  }) => {
    // クリックジャッキング防止 (#2387) の除外が効いていること。ここが
    // 逆転すると、埋め込みが動かないか、アプリ本体が frame 可能になる。
    const embed = await request.get(`${baseURL}/embed/notes/nonexistent`);
    expect(embed.headers()['x-frame-options']).toBeUndefined();

    const app = await request.get(`${baseURL}/`);
    expect(app.headers()['x-frame-options']).toBe('DENY');
  });

  test('非公開の投稿は埋め込まれない', async ({ request, baseURL }) => {
    const me = await signupUser(request, randomUsername('embv'));

    // 埋め込みは **認証なしで誰でも読める** 経路なので、ここが緩むと
    // そのまま IDOR になる。upstream も specified / followers を弾く。
    const cases: Array<{ visibility: 'public' | 'home' | 'followers' | 'specified'; embedded: boolean }> = [
      { visibility: 'public', embedded: true },
      { visibility: 'home', embedded: true },
      { visibility: 'followers', embedded: false },
      { visibility: 'specified', embedded: false },
    ];

    for (const { visibility, embedded } of cases) {
      const text = `embed-vis-${visibility}-${Date.now()}`;
      const note = await createNote(request, me.token, {
        text,
        visibility,
        ...(visibility === 'specified' ? { visibleUserIds: [me.id] } : {}),
      });

      const resp = await request.get(`${baseURL}/embed/notes/${note.id}`);
      expect(resp.status()).toBe(200);
      const html = await resp.text();

      // 本文そのものが HTML に出ていないことまで見る。embedCtx block の
      // 有無だけだと、別経路で本文が混ざっても気付けない。
      expect(html.includes('misskey_embedCtx'), `${visibility}: embedCtx`).toBe(embedded);
      expect(html.includes(text), `${visibility}: 本文の露出`).toBe(embedded);
    }
  });

  test('非公開のクリップは埋め込まれない', async ({ request, baseURL }) => {
    const me = await signupUser(request, randomUsername('embc'));

    const mk = async (isPublic: boolean) => {
      const resp = await callApi(request, 'clips/create', {
        i: me.token,
        name: `embed-clip-${isPublic ? 'pub' : 'priv'}-${Date.now()}`,
        isPublic,
      });
      expect(resp.status()).toBe(200);
      return resp.json();
    };

    const pub = await mk(true);
    const priv = await mk(false);

    const pubHtml = await (await request.get(`${baseURL}/embed/clips/${pub.id}`)).text();
    expect(pubHtml).toContain('misskey_embedCtx');

    // upstream は clip の存在だけを見るため**非公開 clip も埋め込めてしまう**。
    // mk-go は isPublic も見て弾く (意図的な divergence、docs/divergence.md)。
    // 両者の実挙動をそれぞれ固定する。ゆるい assert に寄せると mk-go 側の
    // 防御が外れても気付けない。
    const privHtml = await (await request.get(`${baseURL}/embed/clips/${priv.id}`)).text();
    if (isTsBackend) {
      expect(privHtml).toContain('misskey_embedCtx');
      return;
    }
    expect(privHtml).not.toContain('misskey_embedCtx');
    expect(privHtml).not.toContain(priv.name);
  });
});
