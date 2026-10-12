/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/object-storage page で S3 互換 storage settings form が hydrate
// されることを smoke する spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/object-storage page hydrates storage form', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  // 他 spec に設定を持ち越さないよう必ず元に戻す。
  test.afterEach(async ({ request }) => {
    await callApi(request, 'admin/update-meta', { i: root.token, useObjectStorage: false });
  });

  test('object storage form hydrates with bucket / endpoint inputs', async ({ page, baseURL, request }) => {
    // フォーム本体は object-storage.vue の `<template v-if="useObjectStorage">` でゲートされており、
    // 機能を有効にしないと input が mount されない (この v-if は 2026.6.0 にも
    // あり、本 spec は元から通っていなかった)。meta を立ててから開く。
    await callApi(request, 'admin/update-meta', { i: root.token, useObjectStorage: true });

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/object-storage`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // page title (i18n.ts.objectStorage → "Object Storage") + S3 設定 input
    // (baseUrl / endpoint / region / bucket / prefix / accessKey / secret 等)
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        const inputs = document.querySelectorAll('input').length;
        return text.includes('Object Storage') && inputs >= 3;
      },
      { timeout: 20_000 },
    );
  });
});
