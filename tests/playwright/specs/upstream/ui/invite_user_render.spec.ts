/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /invite page (= user-side invite code creation) で MkButton +
// invite code list が hydrate されることを smoke する spec。authenticated
// page で /admin/invites とは別 (user 自身の発行枠)。

import { readFileSync } from 'node:fs';
import { test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: /invite (user) page hydrates', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  // 招待制でないときは空の表示になる。招待コードの作成は invite_create.spec.ts (招待制にする
  // ので meta の project で流れる)
  test('invite page renders the empty state when registration is open', coversRoutes('/invite'), async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await gotoRoute(page, baseURL!, '/invite');

    // invite.vue は招待制 (disableRegistration) でなければ空の表示 (MkResult) だけを出す。
    // ページの見出し "Invite" と空の表示の文言の両方で、この画面であることを見る。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return text.includes('Invite') && text.includes("There's nothing to see here");
      },
      { timeout: 20_000 },
    );
  });
});
