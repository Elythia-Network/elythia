/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/roles/:id/edit で 既存 role の name を変更 → Save click →
// /api/admin/roles/update round-trip する **真の write-flow** spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /admin/roles/:id/edit update flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('create role via API → edit name → Save → /api/admin/roles/update', async ({
    page,
    baseURL,
    request,
  }) => {
    const initialName = `pwrole-init-${Date.now().toString().slice(-9)}`;
    // mk-go admin/roles/create paramDef は 13 field 必須 (#889): name /
    // description / target / condFormula / isPublic / isModerator /
    // isAdministrator / asBadge / canEditMembersByModerator /
    // displayOrder / policies。color / iconUrl は nullable optional。
    const createResp = await callApi(request, 'admin/roles/create', {
      i: root.token,
      name: initialName,
      description: '',
      color: null,
      iconUrl: null,
      target: 'manual',
      condFormula: { id: '00000000-0000-0000-0000-000000000000', type: 'isLocal' },
      isPublic: false,
      isModerator: false,
      isAdministrator: false,
      isExplorable: false,
      asBadge: false,
      canEditMembersByModerator: false,
      displayOrder: 0,
      policies: {},
    });
    expect(createResp.status()).toBe(200);
    const roleId = (await createResp.json()).id;
    expect(roleId).toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/roles/${roleId}/edit`, {
      waitUntil: 'domcontentloaded',
    });

    await page.waitForFunction(
      (n) => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.value === n);
      },
      initialName,
      { timeout: 20_000 },
    );

    const newName = `pwrole-updated-${Date.now().toString().slice(-9)}`;
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
      (r) => r.url().includes('/api/admin/roles/update') && r.status() < 400,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    const resp = await updateResp;
    expect(resp.status()).toBeLessThan(400);
  });
});
