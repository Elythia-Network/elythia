/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// AiScript のプラグインを画面で入れて動かし、消す (#4072)。
//
// /settings/plugin/install にコードを貼って入れ、投稿フォームのプラグインのボタン (ti-plug) から、
// プラグインが登録したアクションを選ぶと本文が書き換わることを見る。最後に /settings/plugin で消す。
// プラグインは端末の設定 (plugins) に入り API を呼ばないので、readPref で確かめる。権限を持たない
// プラグインにする (権限を書くとトークンの発行の画面に進む)。

import { expect, test } from '@playwright/test';
import { readPref, signinWithState } from '../../../fixtures/settings_table';
import { clickButtonByText, clickButtonWithIcon } from '../../../fixtures/ui_click';
import { postTextValue, waitForPostForm } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { clickMenuItem, freshUser } from '../../../fixtures/ui_timeline';

test.describe('UI: AiScript plugin', () => {
  test.setTimeout(90_000);

  test('install a plugin, run its post form action and uninstall it', coversRoutes('/settings/plugin/install', '/settings/plugin'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwplg');
    const name = `pwplugin${Date.now().toString(36)}`;
    const code = [
      '/// @ 1.0.0',
      '### {',
      `  name: "${name}"`,
      '  version: "1.0.0"',
      '  author: "playwright"',
      '}',
      'Plugin:register_post_form_action("PW plugin action", @(form, update) {',
      '  update("text", "written by the plugin")',
      '})',
    ].join('\n');
    const plugins = async () => ((await readPref(page, 'plugins')) as { name: string }[] | undefined ?? []).map((p) => p.name);

    await signinWithState(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/settings/plugin/install');
    await page.locator('textarea').first().fill(code);
    await clickButtonByText(page, 'Install');
    await expect.poll(plugins, { message: 'プラグインが入らない' }).toContain(name);

    // プラグインは起動のときに読むので、読み込み直してから投稿フォームを開く
    await gotoRoute(page, baseURL!, '/');
    await page.getByTestId('open-post-form').first().click();
    await waitForPostForm(page);
    await clickButtonWithIcon(page, 'i.ti-plug');
    await clickMenuItem(page, 'PW plugin action');
    await expect.poll(() => postTextValue(page), { message: 'プラグインのアクションが本文を書き換えない' }).toBe('written by the plugin');
    await page.keyboard.press('Escape');

    await gotoRoute(page, baseURL!, '/settings/plugin');
    await page.locator('[data-testid="folder-header"]').filter({ hasText: name }).click();
    await clickButtonByText(page, 'Uninstall');
    await page.getByTestId('modal-dialog-ok').click();
    await expect.poll(plugins, { message: 'プラグインが消えない' }).not.toContain(name);
  });
});
