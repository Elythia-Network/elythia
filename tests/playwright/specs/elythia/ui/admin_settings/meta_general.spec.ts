/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 管理画面の設定の表 (#4066): /admin/settings。行はフォームの枠ごとにまとめ、1 回の保存で
// 変わるキーを宣言する。効き目は、利用者の側から読める出力 (/api/meta、manifest.json、
// 各 API) で見る。行の後は、変えたキーを元の値に戻す (fixtures/admin_table.ts。この spec は
// admin/update-meta を書くので meta の project で流れる)。

import { expect, type Page, test } from '@playwright/test';
import { type AdminRowContext, coversMeta, expectApiOk, expectField, expectRadio, fillField, type MetaRow, pickRadio, runMetaRow, saveFolder, setSwitch, switchByLabel } from '../../../../fixtures/admin_table';
import { waitForMountedPage } from '../../../../fixtures/route_check';
import { gotoRoute } from '../../../../fixtures/ui_route';
import { pageBody } from '../../../../fixtures/ui_timeline';
import { callApi } from '../../../../fixtures/api';
import { coversRoutes } from '../../../../fixtures/ui_route';

const ROUTE = '/admin/settings';
const TAG = 'pwmeta';

/** The fields of the About folder: label, `admin/update-meta` key, the value the row sets. */
const ABOUT: [string, string, string][] = [
  ['Instance name', 'name', `${TAG} server`],
  ['Short name', 'shortName', `${TAG}s`],
  ['Instance description', 'description', `${TAG} description`],
  ['Maintainer', 'maintainerName', `${TAG} admin`],
  ['Maintainer email', 'maintainerEmail', 'admin@remote.example'],
  ['Terms of Service URL', 'tosUrl', 'https://remote.example/tos'],
  ['Privacy Policy URL', 'privacyPolicyUrl', 'https://remote.example/privacy'],
  ['Inquiry URL', 'inquiryUrl', 'https://remote.example/inquiry'],
  ['Repository URL', 'repositoryUrl', 'https://remote.example/repo'],
  ['Impressum URL', 'impressumUrl', 'https://remote.example/impressum'],
];

type Fed = { value: string; label: string };
const FED = { all: { value: 'all', label: 'All' }, none: { value: 'none', label: 'None' } } satisfies Record<string, Fed>;

/**
 * A row that turns the switch `label` to the opposite of the stored value and
 * saves the folder. A row must change the value: the form shows no save
 * footer for an unchanged value.
 */
function switchRow(key: string, label: string, folder: string, effect: MetaRow['effect']): MetaRow {
  return {
    name: key, keys: [key], route: ROUTE,
    setup: async ({ request, root }) => !(await expectApiOk(request, 'admin/meta', { i: root.token }))[key],
    set: async ({ page, data }) => {
      await setSwitch(await switchByLabel(page, label), data as boolean);
      await saveFolder(page, folder);
    },
    read: (meta) => meta[key],
    expected: ({ data }: AdminRowContext) => data,
    shows: async ({ page, data }) => { await expect(await switchByLabel(page, label)).toHaveAttribute('data-checked', String(data)); },
    effect,
  };
}

/** The folder (MkFolder) whose header has exactly the text `label`. */
function folderGroup(page: Page, label: string) {
  return pageBody(page).getByTestId('folder-header').filter({ has: page.getByText(label, { exact: true }) }).locator('xpath=ancestor::*[@role="group"][1]');
}

/** Adds a row to a suspended-software list folder. */
async function addSoftware(page: Page, folder: string, software: string): Promise<void> {
  const group = folderGroup(page, folder);
  await group.getByRole('button', { name: 'Add' }).click();
  await group.getByPlaceholder('Software').last().fill(software);
  await group.getByPlaceholder('Version').last().fill('*');
}

const REMOTE_SKIP = 'リモートのサーバー (連合の相手) が要り、このスタックには居ない';

/** `/api/meta` as a client reads it. */
async function publicMeta(request: Parameters<typeof callApi>[0]): Promise<Record<string, any>> {
  return expectApiOk(request, 'meta', { detail: true });
}

const ROWS: MetaRow[] = [
  {
    name: 'About (name, description, maintainer, links)', keys: ABOUT.map(([, k]) => k) as [string, ...string[]], route: ROUTE,
    set: async ({ page }) => {
      for (const [label, , value] of ABOUT) await fillField(page, label, value);
      await saveFolder(page, 'About');
    },
    read: (meta) => ABOUT.map(([, k]) => meta[k]),
    expected: ABOUT.map(([, , v]) => v),
    shows: async ({ page }) => { for (const [label, , value] of ABOUT) await expectField(page, label, value); },
    effect: async ({ request, baseURL }) => {
      const meta = await publicMeta(request);
      for (const [, k, v] of ABOUT) expect(meta[k], `/api/meta の ${k}`).toBe(v);
      const manifest = await (await request.get(`${baseURL}/manifest.json`)).json() as { short_name?: string };
      expect(manifest.short_name, 'manifest.json の short_name').toBe(`${TAG}s`);
    },
  },
  {
    name: 'pinnedUsers', keys: ['pinnedUsers'], route: ROUTE,
    set: async ({ page, user }) => {
      await fillField(page, 'Pinned users', `@${user.username}`);
      await saveFolder(page, 'Pinned users');
    },
    read: (meta) => meta.pinnedUsers,
    expected: ({ user }: { user: { username: string } }) => [`@${user.username}`],
    shows: async ({ page, user }) => expectField(page, 'Pinned users', `@${user.username}`),
    effect: async ({ request, user }) => {
      const pinned = await expectApiOk(request, 'pinned-users', {}) as { id: string }[];
      expect(pinned.map((u) => u.id), 'おすすめの利用者に出ない').toContain(user.id);
    },
  },
  {
    name: 'ServiceWorker (enableServiceWorker, keys)', keys: ['enableServiceWorker', 'swPublicKey', 'swPrivateKey'], route: ROUTE,
    restore: ['enableServiceWorker', ['swPublicKey', 'swPublickey'], 'swPrivateKey'],
    set: async ({ page }) => {
      await setSwitch(await switchByLabel(page, 'Enable Push-Notifications for your Browser'), true);
      await fillField(page, 'Public key', `${TAG}-public`);
      await fillField(page, 'Private key', `${TAG}-private`);
      await saveFolder(page, 'ServiceWorker');
    },
    // 秘密鍵は admin/meta でも伏せ字で返るので、設定済みかで見る
    read: (meta) => [meta.enableServiceWorker, meta.swPublickey, meta.hasSwPrivateKey],
    expected: [true, `${TAG}-public`, true],
    // 秘密鍵は伏せ字で出るので、表示は見ない (設定済みかを read で見ている)
    shows: async ({ page }) => {
      await expect(await switchByLabel(page, 'Enable Push-Notifications for your Browser')).toHaveAttribute('data-checked', 'true');
      await expectField(page, 'Public key', `${TAG}-public`);
    },
    // 秘密鍵は配信の署名にだけ使い、push service の無いこのスタックでは見えない。公開鍵と有効の印は利用者に届く
    effect: async ({ request }) => {
      const meta = await publicMeta(request);
      expect(meta.enableServiceWorker, '/api/meta の enableServiceWorker').toBe(true);
      expect(meta.swPublickey, '/api/meta の swPublickey').toBe(`${TAG}-public`);
    },
  },
  {
    name: 'notesPerOneAd', keys: ['notesPerOneAd'], route: ROUTE,
    set: async ({ page }) => {
      await fillField(page, 'Real-time update ad placement interval (Notes per ad)', '7');
      await saveFolder(page, 'Ad settings');
    },
    read: (meta) => meta.notesPerOneAd,
    expected: 7,
    shows: ({ page }) => expectField(page, 'Real-time update ad placement interval (Notes per ad)', '7'),
    effect: async ({ request }) => { expect((await publicMeta(request)).notesPerOneAd, '/api/meta の notesPerOneAd').toBe(7); },
  },
  {
    name: 'urlPreviewEnabled → false', keys: ['urlPreviewEnabled'], route: ROUTE, restore: ['urlPreviewEnabled'],
    set: async ({ page }) => {
      await setSwitch(await switchByLabel(page, 'Enable URL preview'), false);
      await saveFolder(page, 'URL preview settings');
    },
    read: (meta) => meta.urlPreviewEnabled,
    expected: false,
    shows: async ({ page }) => { await expect(await switchByLabel(page, 'Enable URL preview')).toHaveAttribute('data-checked', 'false'); },
    effect: async ({ request }) => { expect((await publicMeta(request)).enableUrlPreview, '/api/meta の enableUrlPreview').toBe(false); },
  },
  {
    // 値が変わらないと保存の帯が出ないので、今の値と違う方を選ぶ (このスタックの既定は none)
    name: 'federation', keys: ['federation'], route: ROUTE,
    setup: async ({ request, root }) => ((await expectApiOk(request, 'admin/meta', { i: root.token })).federation === 'none' ? FED.all : FED.none),
    set: async ({ page, data }) => {
      await pickRadio(page, 'Behavior', (data as Fed).label);
      await saveFolder(page, 'Federation');
    },
    read: (meta) => meta.federation,
    expected: ({ data }: { data: unknown }) => (data as Fed).value,
    shows: ({ page, data }) => expectRadio(page, 'Behavior', (data as Fed).label),
    effect: async ({ request, data }) => { expect((await publicMeta(request)).federation, '/api/meta の federation').toBe((data as Fed).value); },
  },
  {
    name: 'singleUserMode', keys: ['singleUserMode'], route: ROUTE,
    set: async ({ page }) => {
      await setSwitch(await switchByLabel(page, 'Single user mode'), true);
      await saveFolder(page, 'Single user mode');
    },
    read: (meta) => meta.singleUserMode,
    expected: true,
    shows: async ({ page }) => { await expect(await switchByLabel(page, 'Single user mode')).toHaveAttribute('data-checked', 'true'); },
    // backend は読まない。管理画面のトップが、設定の警告 (bot 対策が無い、など。このスタックでは出る) を出さなくなる
    effect: async ({ page, baseURL }) => {
      // 警告を出すかは、管理画面が admin/meta を読んでから決まる。読み込みを待ってから数える
      const metaLoaded = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/meta', { timeout: 15_000 });
      await gotoRoute(page, baseURL, '/admin');
      await waitForMountedPage(page);
      await metaLoaded;
      await page.waitForTimeout(500);
      // 警告は本文ではなく、管理画面の左のメニューの列に出るので、画面全体で数える
      expect(await page.getByText('Bot protection is not configured.').count(), '設定の警告が出る').toBe(0);
    },
  },
  {
    name: 'URL preview (redirect, size, timeout, UA, proxy, sensitive list)',
    keys: ['urlPreviewAllowRedirect', 'urlPreviewRequireContentLength', 'urlPreviewMaximumContentLength', 'urlPreviewTimeout', 'urlPreviewUserAgent', 'urlPreviewSummaryProxyUrl', 'urlPreviewSensitiveList'],
    route: ROUTE,
    setup: async ({ request, root }) => {
      const meta = await expectApiOk(request, 'admin/meta', { i: root.token });
      return { redirect: !meta.urlPreviewAllowRedirect, length: !meta.urlPreviewRequireContentLength };
    },
    set: async ({ page, data }) => {
      const d = data as { redirect: boolean; length: boolean };
      await setSwitch(await switchByLabel(page, 'Allow URL preview redirection'), d.redirect);
      await setSwitch(await switchByLabel(page, 'Generate the preview only if you could get Content-Length'), d.length);
      await fillField(page, 'Maximum Content-Length (bytes)', '123456');
      await fillField(page, 'Time out when getting preview (ms)', '4321');
      await fillField(page, 'User-Agent', `${TAG}-ua`);
      await fillField(page, 'Proxy endpoints that generate previews', 'https://remote.example/summaly');
      await fillField(page, 'URL to restrict thumbnail display', 'remote.example');
      await saveFolder(page, 'URL preview settings');
    },
    read: (meta) => [meta.urlPreviewAllowRedirect, meta.urlPreviewRequireContentLength, meta.urlPreviewMaximumContentLength, meta.urlPreviewTimeout, meta.urlPreviewUserAgent, meta.urlPreviewSummaryProxyUrl, meta.urlPreviewSensitiveList],
    expected: ({ data }: AdminRowContext) => {
      const d = data as { redirect: boolean; length: boolean };
      return [d.redirect, d.length, 123456, 4321, `${TAG}-ua`, 'https://remote.example/summaly', ['remote.example']];
    },
    shows: async ({ page, data }) => {
      const d = data as { redirect: boolean; length: boolean };
      await expect(await switchByLabel(page, 'Allow URL preview redirection')).toHaveAttribute('data-checked', String(d.redirect));
      await expect(await switchByLabel(page, 'Generate the preview only if you could get Content-Length')).toHaveAttribute('data-checked', String(d.length));
      await expectField(page, 'Maximum Content-Length (bytes)', '123456');
      await expectField(page, 'Time out when getting preview (ms)', '4321');
      await expectField(page, 'User-Agent', `${TAG}-ua`);
      await expectField(page, 'Proxy endpoints that generate previews', 'https://remote.example/summaly');
      await expectField(page, 'URL to restrict thumbnail display', 'remote.example');
    },
    effect: { skip: 'URL のプレビューを取りに行くときにだけ読まれ、取りに行く相手 (外部のページ、summaly のプロキシ) が要る。このスタックには居ない' },
  },
  {
    name: 'federationHosts (specified)', keys: ['federationHosts'], route: ROUTE, restore: ['federation', 'federationHosts'],
    set: async ({ page }) => {
      await pickRadio(page, 'Behavior', 'Specific host');
      await fillField(page, 'Federation allowed servers', 'remote.example');
      await saveFolder(page, 'Federation');
    },
    read: (meta) => [meta.federation, meta.federationHosts],
    expected: ['specified', ['remote.example']],
    shows: ({ page }) => expectField(page, 'Federation allowed servers', 'remote.example'),
    effect: { skip: REMOTE_SKIP },
  },
  {
    name: 'suspended software (deliver, inbox)', keys: ['deliverSuspendedSoftware', 'inboxSuspendedSoftware'], route: ROUTE,
    set: async ({ page }) => {
      await addSoftware(page, 'Suspended Software', `${TAG}deliver`);
      await addSoftware(page, 'Software refused at the inbox', `${TAG}inbox`);
      await saveFolder(page, 'Federation');
    },
    read: (meta) => [meta.deliverSuspendedSoftware?.at(-1), meta.inboxSuspendedSoftware?.at(-1)],
    expected: [{ software: `${TAG}deliver`, versionRange: '*' }, { software: `${TAG}inbox`, versionRange: '*' }],
    shows: async ({ page }) => {
      await expect(folderGroup(page, 'Suspended Software').getByPlaceholder('Software').last()).toHaveValue(`${TAG}deliver`);
      await expect(folderGroup(page, 'Software refused at the inbox').getByPlaceholder('Software').last()).toHaveValue(`${TAG}inbox`);
    },
    effect: { skip: REMOTE_SKIP },
  },
  switchRow('proxyRemoteFiles', 'Proxy remote files', 'Federation', { skip: 'リモートのファイルの URL にだけ効き、リモートの投稿が要る。このスタックには居ない' }),
  switchRow('allowExternalApRedirect', 'Allow redirects for queries via ActivityPub', 'Federation', { skip: REMOTE_SKIP }),
];

test.describe('admin: meta (/admin/settings)', () => {
  test.setTimeout(90_000);
  for (const row of ROWS) {
    test(row.name, { annotation: [...coversRoutes(ROUTE).annotation, ...coversMeta(...row.keys).annotation] }, ({ page, request, baseURL }) => runMetaRow({ page, request, baseURL }, row));
  }
});
