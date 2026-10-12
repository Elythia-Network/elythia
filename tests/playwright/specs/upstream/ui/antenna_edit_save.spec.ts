/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/antennas/:id で 既存 antenna の name を変更 → Save click →
// /api/antennas/update round-trip する **真の write-flow** spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { deleteAntennasNamed } from '../../../fixtures/quota';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /my/antennas/:id update flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  // 作った antenna は片付ける。root を共有しているので放置すると
  // antennaLimit (既定 5) を使い切って無関係な spec が create で落ちる (#2264)。
  const createdAntennas: string[] = [];
  test.afterEach(async ({ request }) => {
    await deleteAntennasNamed(request, root.token, createdAntennas);
    createdAntennas.length = 0;
  });

  test('create antenna via API → edit name → Save → /api/antennas/update', async ({
    page,
    baseURL,
    request,
  }) => {
    const initialName = `pwant-init-${Date.now().toString().slice(-9)}`;
    createdAntennas.push(initialName);
    const createResp = await callApi(request, 'antennas/create', {
      i: root.token,
      name: initialName,
      src: 'all',
      keywords: [['*']],
      excludeKeywords: [],
      // upstream の paramDef は users も required なので、省くと 400
      // INVALID_PARAM になる。mk-go は必須にしていないため mk-go 単体では
      // 通っていたが、TS backend で落ちていた (#2276)。
      users: [],
      caseSensitive: false,
      withReplies: false,
      withFile: false,
      localOnly: false,
    });
    expect(createResp.status()).toBe(200);
    const antennaId = (await createResp.json()).id;
    expect(antennaId).toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/my/antennas/${antennaId}`, {
      waitUntil: 'domcontentloaded',
    });

    // antenna name MkInput が initialName で hydrate
    await page.waitForFunction(
      (n) => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.value === n);
      },
      initialName,
      { timeout: 20_000 },
    );

    const newName = `pwant-updated-${Date.now().toString().slice(-9)}`;
    createdAntennas.push(newName);
    await page.evaluate(
      ({ from, to }) => {
        const target = (
          Array.from(document.querySelectorAll('input')) as HTMLInputElement[]
        ).find((i) => i.value === from);
        if (!target) return;
        target.focus();
        const setter = Object.getOwnPropertyDescriptor(
          window.HTMLInputElement.prototype,
          'value',
        )?.set;
        setter?.call(target, to);
        target.dispatchEvent(new Event('input', { bubbles: true }));
      },
      { from: initialName, to: newName },
    );

    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/antennas/update') && r.status() < 400,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    const resp = await updateResp;
    expect(resp.status()).toBeLessThan(400);
  });
});
