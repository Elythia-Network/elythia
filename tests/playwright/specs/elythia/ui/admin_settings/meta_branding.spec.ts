/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 管理画面の設定の表 (#4066): /admin/branding。画面の下の Save で全部をまとめて保存する
// 作りなので、1 行で全部を変えて保存し、効き目は利用者の側から読める出力 (/api/meta、
// manifest.json) で見る。行の後は元の値に戻す (この spec は meta の project で流れる)。

import { expect, test } from '@playwright/test';
import { type AdminRowContext, coversMeta, expectApiOk, expectField, expectRadio, fillField, type MetaRow, pickRadio, runMetaRow, savePage, setSwitch, switchByLabel } from '../../../../fixtures/admin_table';
import { coversRoutes } from '../../../../fixtures/ui_route';

const ROUTE = '/admin/branding';
const URL_BASE = 'https://remote.example/pwbrand';

/** The URL fields: label, `admin/update-meta` key, the value the row sets. */
const URLS: [string, string, string][] = [
  ['Icon URL', 'iconUrl', `${URL_BASE}/icon.png`],
  ['Icon URL (App/192px)', 'app192IconUrl', `${URL_BASE}/192.png`],
  ['Icon URL (App/512px)', 'app512IconUrl', `${URL_BASE}/512.png`],
  ['Banner image URL', 'bannerUrl', `${URL_BASE}/banner.png`],
  ['Background image URL', 'backgroundImageUrl', `${URL_BASE}/bg.png`],
  ['No page corresponding to this URL could be found.', 'notFoundImageUrl', `${URL_BASE}/404.png`],
  ["There's nothing to see here", 'infoImageUrl', `${URL_BASE}/info.png`],
  ['An error has occurred', 'serverErrorImageUrl', `${URL_BASE}/error.png`],
  ['Repository URL', 'repositoryUrl', `${URL_BASE}/repo`],
  ['Feedback URL', 'feedbackUrl', `${URL_BASE}/feedback`],
];
const LIGHT = '{"id":"pw-light","name":"pw light","base":"light","props":{}}';
const DARK = '{"id":"pw-dark","name":"pw dark","base":"dark","props":{}}';
const OVERRIDE = '{"description":"pwbrand manifest"}';

/** The entrance page style that differs from the stored one (the form must change to save). */
type Style = 'classic' | 'simple';

const ROWS: MetaRow[] = [
  {
    name: 'branding (entrance, icons, images, theme, links, manifest)',
    keys: ['clientOptions', ...URLS.map(([, k]) => k), 'themeColor', 'defaultLightTheme', 'defaultDarkTheme', 'manifestJsonOverride'] as [string, ...string[]],
    route: ROUTE,
    setup: async ({ request, root }) => {
      const meta = await expectApiOk(request, 'admin/meta', { i: root.token });
      const style: Style = meta.clientOptions?.entrancePageStyle === 'simple' ? 'classic' : 'simple';
      return { style, timeline: !meta.clientOptions?.showTimelineForVisitor };
    },
    set: async ({ page, data }) => {
      const d = data as { style: Style; timeline: boolean };
      await pickRadio(page, 'Entrance page style', d.style);
      await setSwitch(await switchByLabel(page, 'Show timeline'), d.timeline);
      for (const [label, , value] of URLS) await fillField(page, label, value);
      await fillField(page, 'Instance Ticker Color', '#123456');
      await fillField(page, 'Instance-wide default light theme', LIGHT);
      await fillField(page, 'Instance-wide default dark theme', DARK);
      await fillField(page, 'manifest.json Override', OVERRIDE);
      await savePage(page);
    },
    read: (meta) => [meta.clientOptions?.entrancePageStyle, meta.clientOptions?.showTimelineForVisitor, ...URLS.map(([, k]) => meta[k]), meta.themeColor, meta.defaultLightTheme, meta.defaultDarkTheme, meta.manifestJsonOverride],
    expected: ({ data }: AdminRowContext) => {
      const d = data as { style: Style; timeline: boolean };
      return [d.style, d.timeline, ...URLS.map(([, , v]) => v), '#123456', LIGHT, DARK, OVERRIDE];
    },
    shows: async ({ page, data }) => {
      const d = data as { style: Style; timeline: boolean };
      await expectRadio(page, 'Entrance page style', d.style);
      await expect(await switchByLabel(page, 'Show timeline')).toHaveAttribute('data-checked', String(d.timeline));
      for (const [label, , value] of URLS) await expectField(page, label, value);
      await expectField(page, 'Instance Ticker Color', '#123456');
      await expectField(page, 'Instance-wide default light theme', LIGHT);
      await expectField(page, 'Instance-wide default dark theme', DARK);
      // 画面は保存した JSON をタブで字下げして出す
      await expectField(page, 'manifest.json Override', JSON.stringify(JSON.parse(OVERRIDE), null, '\t'));
    },
    effect: async ({ request, baseURL, data }) => {
      const d = data as { style: Style; timeline: boolean };
      const meta = await expectApiOk(request, 'meta', { detail: true });
      expect(meta.clientOptions?.entrancePageStyle, '/api/meta の clientOptions.entrancePageStyle').toBe(d.style);
      expect(meta.clientOptions?.showTimelineForVisitor, '/api/meta の clientOptions.showTimelineForVisitor').toBe(d.timeline);
      // app192IconUrl / app512IconUrl は /api/meta に出ず、manifest.json のアイコンになる
      for (const [, k, v] of URLS) if (!k.startsWith('app')) expect(meta[k], `/api/meta の ${k}`).toBe(v);
      expect(meta.themeColor, '/api/meta の themeColor').toBe('#123456');
      expect(meta.defaultLightTheme, '/api/meta の defaultLightTheme').toBe(LIGHT);
      expect(meta.defaultDarkTheme, '/api/meta の defaultDarkTheme').toBe(DARK);
      const manifest = await (await request.get(`${baseURL}/manifest.json`)).json() as { icons?: { src: string }[]; theme_color?: string; description?: string };
      const icons = (manifest.icons ?? []).map((i) => i.src);
      expect(icons, 'manifest.json の 192px のアイコン').toContain(`${URL_BASE}/192.png`);
      expect(icons, 'manifest.json の 512px のアイコン').toContain(`${URL_BASE}/512.png`);
      expect(manifest.theme_color, 'manifest.json の theme_color').toBe('#123456');
      expect(manifest.description, 'manifest.json の上書き').toBe('pwbrand manifest');
    },
  },
];

test.describe('admin: meta (/admin/branding)', () => {
  test.setTimeout(90_000);
  for (const row of ROWS) {
    test(row.name, { annotation: [...coversRoutes(ROUTE).annotation, ...coversMeta(...row.keys).annotation] }, ({ page, request, baseURL }) => runMetaRow({ page, request, baseURL }, row));
  }
});
