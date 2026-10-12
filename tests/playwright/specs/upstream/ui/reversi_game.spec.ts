/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /reversi/g/:gameId (対局画面) をブラウザで開く (#2441)。
//
// 既存の `reversi_lobby_render.spec.ts` はロビーまで。対局画面は未検証だった。
//
// 対局を作るにはマッチメイキングが要る。`reversi/match` は
//
//   1. 先に呼んだ側 (root) が招待を作り **204** を返す
//   2. 相手が同じ相手を指定して呼ぶと、その招待を accept 扱いにして
//      **200 + game** を返す
//
// という二段構え。片側だけ呼んでも対局は成立しない。
//
// 対局画面は `game.isStarted` が false の間は **設定画面 (GameSetting)** を出す。
// さらに `connection == null` の間はローディングのままなので、**streaming の
// 接続が確立して初めて中身が出る**。ここが出るということは
// `reversi/show-game` と reversiGame チャンネルの両方が生きている。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /reversi/g/:gameId', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(90_000);

  test('マッチングした対局の設定画面が表示される', async ({ page, baseURL, request }) => {
    const peer = await signupUser(request, randomUsername('rvsi'), DEFAULT_TEST_PASSWORD);

    // root から招待 (204)。ここで 200 が返るなら二段構えの前提が崩れている。
    const invited = await callApi(request, 'reversi/match', {
      i: root.token,
      userId: peer.id,
    });
    expect(invited.status()).toBe(204);

    // 相手が同じ相手を指定して呼ぶと成立し、game が返る。
    const matched = await callApi(request, 'reversi/match', {
      i: peer.token,
      userId: root.id,
    });
    expect(matched.status()).toBe(200);
    const game = (await matched.json()) as { id: string; isStarted: boolean };
    expect(game.isStarted).toBe(false);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/reversi/g/${game.id}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // 未開始なので設定画面。ここが出れば reversi/show-game と reversiGame
    // チャンネルの両方が生きている (どちらかが死ぬとローディングのまま止まる)。
    await expect(page.getByText('Game settings', { exact: false }).first()).toBeVisible({
      timeout: 30_000,
    });
    await expect(page.getByText('Black/White', { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('存在しない対局 ID でも画面が壊れない', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/reversi/g/aaaaaaaaaaaaaaaa`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // 終了した対局への古いリンクを踏む経路。SPA が生きていれば他へ移動できる。
    await expect(page.getByText('Timeline', { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });
});
