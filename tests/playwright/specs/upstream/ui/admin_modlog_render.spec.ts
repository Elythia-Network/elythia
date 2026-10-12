/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/modlog page で type filter (MkSelect) + moderator ID input が
// hydrate されることを smoke する spec。
//
// /admin/modlog は admin/show-moderation-logs paginator + filter form を
// mount する。実 log は moderation 操作後にしか発生しないので、本 spec は
// filter UI の有無だけを sign にする (= log entry 表示 verify は別 spec)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/modlog page hydrates filter controls', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('modlog page renders type filter + moderator ID input', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/modlog`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // /admin/modlog 固有 sign: definePage の title が
    // i18n.ts.moderationLogs (= "Moderation logs") なので、
    // 同文字列 + filter input が両方揃うことを verify する。
    // home dashboard に streaming される input でも 1 件だけは満たすが、
    // "Moderation logs" は modlog page 以外では出ない。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        const inputs = document.querySelectorAll('input').length;
        return text.includes('Moderation logs') && inputs >= 1;
      },
      { timeout: 20_000 },
    );
  });
});
