/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #744 Phase 1 smoke: globalSetup が root を用意した状態で /api/i が
// upstream Misskey TS と同じ shape を返すことを確認する。
//
// PR-1 の構成では本 spec が直接 admin/accounts/create を叩いていたが、
// PR-2 で globalSetup に admin 系 setup を集約した (= 複数 spec が rate
// limit や disableRegistration の制約に引っかからないように)。本 spec は
// globalSetup の動作確認も兼ねる: root credentials が `.auth/root.json` に
// 書き出され、それを使って /api/i が valid response を返せること。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

test.describe('smoke: root via globalSetup + /api/i', () => {
  test('root credentials persist and hydrate via /api/i', async ({ request }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    expect(root.id).toBeTruthy();
    expect(root.token).toBeTruthy();
    expect(root.username).toBe('alice');

    const resp = await callApi(request, 'i', { i: root.token });
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.id).toBe(root.id);
    expect(body.username).toBe('alice');
  });
});
