/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定の表 (#4056): /settings/sounds と /settings/navbar の端末の設定。
//
// 音の設定は、部品で値を変えて保存と再読み込みを確かめるところまでにする。効き目は
// 効果音を鳴らすかどうかと音量で、ブラウザの中で鳴った音は確かめられない。
// ナビのサブボタンは、universal の UI のナビ (幅 1280px では折りたたまれない) で見る。

import { expect, test } from '@playwright/test';
import { gotoRoute } from '../../../../fixtures/ui_route';
import { coversPrefs, type PrefRow, prefTitle, runPrefRow } from '../../../../fixtures/settings_table';

test.describe('settings: sounds and navbar', () => {
  test.setTimeout(60_000);
  const ROWS: PrefRow[] = [
    {
      key: 'sound.notUseSound', route: '/settings/sounds', control: { kind: 'switch', on: true }, value: true,
      effect: { skip: '音を鳴らさないだけ (utility/sound.ts の isMute)。鳴った音はブラウザの中で確かめられない' },
    },
    {
      key: 'sound.useSoundOnlyWhenActive', route: '/settings/sounds', control: { kind: 'switch', on: true }, value: true,
      effect: { skip: 'タブが隠れている (visibilityState が hidden) ときに音を鳴らさないだけ (utility/sound.ts)。鳴った音は確かめられない' },
    },
    {
      key: 'sound.masterVolume', route: '/settings/sounds', control: { kind: 'range', value: 1, min: 0, max: 1 }, value: 1,
      effect: { skip: '効果音の音量 (utility/sound.ts、0 なら鳴らさない)。鳴った音の大きさは確かめられない' },
    },
    {
      key: 'showNavbarSubButtons', route: '/settings/navbar', control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page, baseURL }) => {
        await gotoRoute(page, baseURL, '/');
        // ナビが出ていることを先に確かめてから、サブボタン (メニューの編集の ti-settings-2) が無いことを見る
        await expect(page.getByTestId('open-post-form').first()).toBeVisible();
        await expect(page.locator('button:has(> i.ti-settings-2)')).toHaveCount(0);
      },
    },
  ];
  for (const row of ROWS) test(prefTitle(row), coversPrefs(row.key), ({ page, request, baseURL }) => runPrefRow({ page, request, baseURL }, row));
});
