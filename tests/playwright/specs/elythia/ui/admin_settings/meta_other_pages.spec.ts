/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 管理画面の設定の表 (#4066): セキュリティ・性能・オブジェクトストレージ・メール・外部サービス・
// リレー・サーバーのルール (モデレーションの画面の中の枠)。外部のサービス (S3、SMTP、判定の API、メールの検証) やリモートの
// サーバーが要るものは、保存と表示までを確かめ、効き目は理由を書いて省く。行の後は元の値に
// 戻す (この spec は admin/update-meta を書くので meta の project で流れる)。

import { expect, type Page, test } from '@playwright/test';
import { type AdminRowContext, coversMeta, expectApiOk, expectField, fillField, type MetaRow, pickRadio, rangeToMax, runMetaRow, saveFolder, savePage, setSwitch, switchByLabel, switchIn } from '../../../../fixtures/admin_table';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { pageBody } from '../../../../fixtures/ui_timeline';
import { coversRoutes } from '../../../../fixtures/ui_route';

/** The folder (MkFolder) whose header has exactly the text `label`. */
function folderGroup(page: Page, label: string) {
  return pageBody(page).getByTestId('folder-header').filter({ has: page.getByText(label, { exact: true }) }).locator('xpath=ancestor::*[@role="group"][1]');
}

/** Runs `action` and waits for the `admin/update-meta` it sends (the switches that save as soon as they change). */
async function savedBy(page: Page, action: () => Promise<void>): Promise<void> {
  const saved = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/update-meta', { timeout: 15_000 });
  await action();
  expect((await saved).ok(), 'admin/update-meta が失敗した').toBe(true);
}

/** Reads the stored values of `keys` and returns their opposites (a form saves only a changed value). */
function opposites(keys: string[]): (ctx: AdminRowContext) => Promise<Record<string, boolean>> {
  return async ({ request, root }) => {
    const meta = await expectApiOk(request, 'admin/meta', { i: root.token });
    return Object.fromEntries(keys.map((k) => [k, !meta[k]]));
  };
}

const EXTERNAL = (what: string) => `${what}が要り、このスタックには無い。保存と表示までを確かめる`;
const REMOTE = 'リモートのサーバー (連合の相手、リレー) が要り、このスタックには居ない。保存と表示までを確かめる';

const SECURITY = '/admin/security';
const PERFORMANCE = '/admin/performance';

const ROWS: MetaRow[] = [
  // --- /admin/security
  {
    name: 'sensitive media detection', route: SECURITY,
    keys: ['sensitiveMediaDetection', 'sensitiveMediaDetectionSensitivity', 'setSensitiveFlagAutomatically', 'enableSensitiveMediaDetectionForVideos', 'sensitiveMediaDetectionApiUrl', 'sensitiveMediaDetectionApiKey', 'sensitiveMediaDetectionTimeout', 'sensitiveMediaDetectionMaxImagesPerRequest'],
    setup: opposites(['setSensitiveFlagAutomatically', 'enableSensitiveMediaDetectionForVideos']),
    set: async ({ page, data }) => {
      const d = data as Record<string, boolean>;
      const folder = folderGroup(page, 'Detection of sensitive media');
      await pickRadio(page, 'Detection of sensitive media', 'All').catch(async () => {
        // MkRadios に見出しが無いので、枠の中の選択肢をそのまま押す
        await folder.locator('[role="checkbox"]').filter({ hasText: /^\s*All\s*$/ }).click();
      });
      await fillField(page, 'Detection service endpoint URL', 'https://remote.example/detector');
      await fillField(page, 'API key', 'pw-detector-key');
      await fillField(page, 'Timeout (Milliseconds)', '4321');
      await fillField(page, 'Max images per request', '3');
      await rangeToMax(page, folder.locator('.timctyfi').first());
      await setSwitch(await switchIn(folder, 'Enable analysis of videos'), d.enableSensitiveMediaDetectionForVideos);
      await setSwitch(await switchIn(folder, 'Mark as sensitive'), d.setSensitiveFlagAutomatically);
      await saveFolder(page, 'Detection of sensitive media');
    },
    read: (meta) => [meta.sensitiveMediaDetection, meta.sensitiveMediaDetectionSensitivity, meta.setSensitiveFlagAutomatically, meta.enableSensitiveMediaDetectionForVideos, meta.sensitiveMediaDetectionApiUrl, meta.hasSensitiveMediaDetectionApiKey, meta.sensitiveMediaDetectionTimeout, meta.sensitiveMediaDetectionMaxImagesPerRequest],
    expected: ({ data }: AdminRowContext) => {
      const d = data as Record<string, boolean>;
      return ['all', 'veryHigh', d.setSensitiveFlagAutomatically, d.enableSensitiveMediaDetectionForVideos, 'https://remote.example/detector', true, 4321, 3];
    },
    // API の鍵は伏せ字で出るので、表示は見ない (設定済みかを read で見ている)。感度の MkRange は値を出さないので read で見る
    shows: async ({ page, data }) => {
      const d = data as Record<string, boolean>;
      const folder = folderGroup(page, 'Detection of sensitive media');
      await expect(folder.locator('[role="checkbox"]').filter({ hasText: /^\s*All\s*$/ })).toHaveAttribute('aria-checked', 'true');
      await expectField(page, 'Detection service endpoint URL', 'https://remote.example/detector');
      await expectField(page, 'Timeout (Milliseconds)', '4321');
      await expectField(page, 'Max images per request', '3');
      await expect(await switchIn(folder, 'Enable analysis of videos')).toHaveAttribute('data-checked', String(d.enableSensitiveMediaDetectionForVideos));
      await expect(await switchIn(folder, 'Mark as sensitive')).toHaveAttribute('data-checked', String(d.setSensitiveFlagAutomatically));
    },
    effect: { skip: EXTERNAL('判定のサービス (sensitive-detector)') },
  },
  {
    name: 'active email validation', route: SECURITY,
    keys: ['enableActiveEmailValidation', 'enableVerifymailApi', 'verifymailAuthKey', 'enableTruemailApi', 'truemailInstance', 'truemailAuthKey'],
    setup: opposites(['enableActiveEmailValidation']),
    set: async ({ page, data }) => {
      const folder = folderGroup(page, 'Active Email Validation');
      await setSwitch(await switchIn(folder, 'Enable'), (data as Record<string, boolean>).enableActiveEmailValidation);
      await setSwitch(await switchIn(folder, 'Use Verifymail.io API'), true);
      await setSwitch(await switchIn(folder, 'Use TrueMail API'), true);
      await fillField(page, 'Verifymail.io API Auth Key', 'pw-verifymail');
      await fillField(page, 'TrueMail API Instance', 'https://remote.example/truemail');
      await fillField(page, 'TrueMail API Auth Key', 'pw-truemail');
      await saveFolder(page, 'Active Email Validation');
    },
    read: (meta) => [meta.enableActiveEmailValidation, meta.enableVerifymailApi, meta.hasVerifymailAuthKey, meta.enableTruemailApi, meta.truemailInstance, meta.hasTruemailAuthKey],
    expected: ({ data }: AdminRowContext) => [(data as Record<string, boolean>).enableActiveEmailValidation, true, true, true, 'https://remote.example/truemail', true],
    // 鍵は伏せ字で出るので、表示は見ない (設定済みかを read で見ている)
    shows: async ({ page, data }) => {
      const folder = folderGroup(page, 'Active Email Validation');
      await expect(await switchIn(folder, 'Enable')).toHaveAttribute('data-checked', String((data as Record<string, boolean>).enableActiveEmailValidation));
      await expect(await switchIn(folder, 'Use Verifymail.io API')).toHaveAttribute('data-checked', 'true');
      await expect(await switchIn(folder, 'Use TrueMail API')).toHaveAttribute('data-checked', 'true');
      await expectField(page, 'TrueMail API Instance', 'https://remote.example/truemail');
    },
    effect: { skip: EXTERNAL('メールの検証 (DNS の MX、Verifymail.io、TrueMail)') },
  },
  {
    name: 'bannedEmailDomains', keys: ['bannedEmailDomains'], route: SECURITY,
    set: async ({ page }) => {
      await fillField(page, 'Banned Email Domains List', 'pwbanned.example');
      await saveFolder(page, 'Banned Email Domains');
    },
    read: (meta) => meta.bannedEmailDomains,
    expected: ['pwbanned.example'],
    shows: ({ page }) => expectField(page, 'Banned Email Domains List', 'pwbanned.example'),
    effect: async ({ request }) => {
      const r = await expectApiOk(request, 'email-address/available', { emailAddress: 'pw@pwbanned.example' });
      expect(r, '使えないドメインのメールアドレスが使えるとされる').toEqual({ available: false, reason: 'banned' });
    },
  },
  {
    name: 'enableIpLogging', keys: ['enableIpLogging'], route: SECURITY,
    set: async ({ page }) => {
      await setSwitch(await switchIn(folderGroup(page, 'Log IP address'), 'Enable'), true);
      await saveFolder(page, 'Log IP address');
    },
    read: (meta) => meta.enableIpLogging,
    expected: true,
    shows: async ({ page }) => { await expect(await switchIn(folderGroup(page, 'Log IP address'), 'Enable')).toHaveAttribute('data-checked', 'true'); },
    // 有効にした後の API の呼び出しで、利用者の IP が記録される
    effect: async ({ request, root }) => {
      const u = await signupUser(request, randomUsername('pwip'));
      await expectApiOk(request, 'i', { i: u.token });
      const ips = await expectApiOk(request, 'admin/get-user-ips', { i: root.token, userId: u.id }) as unknown[];
      expect(ips.length, 'IP が記録されない').toBeGreaterThan(0);
    },
  },
  // --- /admin/performance
  {
    name: 'enableServerMachineStats', keys: ['enableServerMachineStats'], route: PERFORMANCE,
    setup: opposites(['enableServerMachineStats']),
    set: async ({ page, data }) => { await savedBy(page, async () => setSwitch(await switchByLabel(page, 'Publish server hardware stats'), (data as Record<string, boolean>).enableServerMachineStats)); },
    read: (meta) => meta.enableServerMachineStats,
    expected: ({ data }: AdminRowContext) => (data as Record<string, boolean>).enableServerMachineStats,
    shows: async ({ page, data }) => { await expect(await switchByLabel(page, 'Publish server hardware stats')).toHaveAttribute('data-checked', String((data as Record<string, boolean>).enableServerMachineStats)); },
    // 公開しないと、server-info のマシンの名前が「?」になる
    effect: async ({ request, data }) => {
      const info = await expectApiOk(request, 'server-info', {}) as { machine: string };
      if ((data as Record<string, boolean>).enableServerMachineStats) expect(info.machine, 'マシンの情報が公開されない').not.toBe('?');
      else expect(info.machine, 'マシンの情報が伏せられない').toBe('?');
    },
  },
  {
    name: 'enableIdenticonGeneration → false', keys: ['enableIdenticonGeneration'], route: PERFORMANCE,
    set: async ({ page }) => { await savedBy(page, async () => setSwitch(await switchByLabel(page, 'Enable user identicon generation'), false)); },
    read: (meta) => meta.enableIdenticonGeneration,
    expected: false,
    shows: async ({ page }) => { await expect(await switchByLabel(page, 'Enable user identicon generation')).toHaveAttribute('data-checked', 'false'); },
    // 作らないと、/identicon は利用者ごとの絵ではなく小さな既定の画像を返す
    effect: async ({ request, baseURL }) => {
      const body = await (await request.get(`${baseURL}/identicon/pw${Date.now().toString(36)}`)).body();
      expect(body.length, 'identicon が作られる').toBeLessThan(500);
    },
  },
  {
    name: 'remote charts and badges', route: PERFORMANCE,
    keys: ['enableChartsForRemoteUser', 'enableStatsForFederatedInstances', 'enableChartsForFederatedInstances', 'showRoleBadgesOfRemoteUsers'],
    setup: opposites(['enableChartsForRemoteUser', 'enableStatsForFederatedInstances', 'enableChartsForFederatedInstances', 'showRoleBadgesOfRemoteUsers']),
    set: async ({ page, data }) => {
      const d = data as Record<string, boolean>;
      for (const [label, key] of PERF_SWITCHES) await savedBy(page, async () => setSwitch(await switchByLabel(page, label), d[key]));
    },
    read: (meta) => PERF_SWITCHES.map(([, k]) => meta[k]),
    expected: ({ data }: AdminRowContext) => PERF_SWITCHES.map(([, k]) => (data as Record<string, boolean>)[k]),
    shows: async ({ page, data }) => {
      for (const [label, key] of PERF_SWITCHES) await expect(await switchByLabel(page, label)).toHaveAttribute('data-checked', String((data as Record<string, boolean>)[key]));
    },
    effect: { skip: REMOTE },
  },
  {
    name: 'FTT (fan-out timeline)', route: PERFORMANCE,
    keys: ['enableFanoutTimeline', 'enableFanoutTimelineDbFallback', 'perLocalUserUserTimelineCacheMax', 'perRemoteUserUserTimelineCacheMax', 'perUserHomeTimelineCacheMax', 'perUserListTimelineCacheMax'],
    setup: opposites(['enableFanoutTimeline', 'enableFanoutTimelineDbFallback']),
    set: async ({ page, data }) => {
      const d = data as Record<string, boolean>;
      const folder = folderGroup(page, 'Misskey® Fan-out Timeline Technology™ (FTT)');
      // フォールバックと上限の欄は、FTT が有効のときだけ出る。先に有効にして値を入れ、最後に行の値にする
      await setSwitch(await switchIn(folder, 'Enable'), true);
      await setSwitch(await switchIn(folder, 'Fallback to database'), d.enableFanoutTimelineDbFallback);
      for (const [label, value] of FTT_FIELDS) await fillField(page, label, value);
      await setSwitch(await switchIn(folder, 'Enable'), d.enableFanoutTimeline);
      await saveFolder(page, 'Misskey® Fan-out Timeline Technology™ (FTT)');
    },
    read: (meta) => [meta.enableFanoutTimeline, meta.enableFanoutTimelineDbFallback, ...FTT_FIELDS.map(([k]) => meta[k])],
    expected: ({ data }: AdminRowContext) => {
      const d = data as Record<string, boolean>;
      return [d.enableFanoutTimeline, d.enableFanoutTimelineDbFallback, ...FTT_FIELDS.map(([, v]) => Number(v))];
    },
    shows: async ({ page, data }) => {
      const d = data as Record<string, boolean>;
      const folder = folderGroup(page, 'Misskey® Fan-out Timeline Technology™ (FTT)');
      await expect(await switchIn(folder, 'Enable')).toHaveAttribute('data-checked', String(d.enableFanoutTimeline));
      if (!d.enableFanoutTimeline) await setSwitch(await switchIn(folder, 'Enable'), true);
      await expect(await switchIn(folder, 'Fallback to database')).toHaveAttribute('data-checked', String(d.enableFanoutTimelineDbFallback));
      for (const [label, value] of FTT_FIELDS) await expectField(page, label, value);
    },
    // FTT を切ると DB から読む経路に変わるが、タイムラインの中身は同じで外からは見えない。上限は件数が溜まらないと効かない
    effect: { skip: 'タイムラインを Redis から読むか DB から読むかの内部の経路だけが変わり、外から見える結果は同じ。上限は件数が上限まで溜まらないと効かない' },
  },
  {
    name: 'enableReactionsBuffering', keys: ['enableReactionsBuffering'], route: PERFORMANCE,
    setup: opposites(['enableReactionsBuffering']),
    set: async ({ page, data }) => {
      await setSwitch(await switchIn(folderGroup(page, 'Misskey® Reactions Boost Technology™ (RBT)'), 'Enable'), (data as Record<string, boolean>).enableReactionsBuffering);
      await saveFolder(page, 'Misskey® Reactions Boost Technology™ (RBT)');
    },
    read: (meta) => meta.enableReactionsBuffering,
    expected: ({ data }: AdminRowContext) => (data as Record<string, boolean>).enableReactionsBuffering,
    shows: async ({ page, data }) => { await expect(await switchIn(folderGroup(page, 'Misskey® Reactions Boost Technology™ (RBT)'), 'Enable')).toHaveAttribute('data-checked', String((data as Record<string, boolean>).enableReactionsBuffering)); },
    effect: { skip: 'リアクションの数を DB に書くまで Redis に溜めるかの内部の経路だけが変わり、利用者から見える数は同じ' },
  },
  {
    name: 'remote notes cleaning', route: PERFORMANCE,
    keys: ['enableRemoteNotesCleaning', 'remoteNotesCleaningExpiryDaysForEachNotes', 'remoteNotesCleaningMaxProcessingDurationInMinutes'],
    setup: opposites(['enableRemoteNotesCleaning']),
    set: async ({ page, data }) => {
      const folder = folderGroup(page, 'Remote Notes Cleaning (仮)');
      // 日数と時間の欄は、有効のときだけ出る。先に有効にして値を入れ、最後に行の値 (今と逆) にする
      await setSwitch(await switchIn(folder, 'Enable'), true);
      await fillField(page, 'Minimum days to retain notes', '45');
      await fillField(page, 'Maximum cleanup processing time', '12');
      await setSwitch(await switchIn(folder, 'Enable'), (data as Record<string, boolean>).enableRemoteNotesCleaning);
      await saveFolder(page, 'Remote Notes Cleaning (仮)');
    },
    read: (meta) => [meta.enableRemoteNotesCleaning, meta.remoteNotesCleaningExpiryDaysForEachNotes, meta.remoteNotesCleaningMaxProcessingDurationInMinutes],
    expected: ({ data }: AdminRowContext) => [(data as Record<string, boolean>).enableRemoteNotesCleaning, 45, 12],
    shows: async ({ page, data }) => {
      const on = (data as Record<string, boolean>).enableRemoteNotesCleaning;
      const folder = folderGroup(page, 'Remote Notes Cleaning (仮)');
      await expect(await switchIn(folder, 'Enable')).toHaveAttribute('data-checked', String(on));
      if (!on) await setSwitch(await switchIn(folder, 'Enable'), true);
      await expectField(page, 'Minimum days to retain notes', '45');
      await expectField(page, 'Maximum cleanup processing time', '12');
    },
    effect: { skip: REMOTE },
  },
  // --- /admin/object-storage
  {
    name: 'object storage and chunked upload', route: '/admin/object-storage',
    keys: ['useObjectStorage', 'objectStorageBaseUrl', 'objectStorageBucket', 'objectStoragePrefix', 'objectStorageEndpoint', 'objectStorageRegion', 'objectStorageAccessKey', 'objectStorageSecretKey', 'objectStorageUseSSL', 'objectStorageUseProxy', 'objectStorageSetPublicRead', 'objectStorageS3ForcePathStyle', 'chunkedUploadEnabled', 'chunkedUploadChunkSizeMb', 'chunkedUploadSessionTtlMinutes'],
    setup: opposites(['objectStorageUseSSL', 'objectStorageUseProxy', 'objectStorageSetPublicRead', 'objectStorageS3ForcePathStyle', 'chunkedUploadEnabled']),
    set: async ({ page, data }) => {
      const d = data as Record<string, boolean>;
      await setSwitch(await switchByLabel(page, 'Use object storage'), true);
      for (const [label, , value] of S3_FIELDS) await fillField(page, label, value);
      for (const [label, key] of S3_SWITCHES) await setSwitch(await switchByLabel(page, label), d[key]);
      const chunked = folderGroup(page, 'Enable chunked uploads');
      if ((await chunked.getAttribute('aria-expanded')) !== 'true') await chunked.getByTestId('folder-header').first().click();
      await setSwitch(await switchIn(chunked, 'Enable chunked uploads'), d.chunkedUploadEnabled);
      await fillField(page, 'Chunk size (MiB)', '7');
      await fillField(page, 'Session lifetime (minutes)', '33');
      await savePage(page);
    },
    read: (meta) => [meta.useObjectStorage, ...S3_FIELDS.map(([, k]) => (k === 'objectStorageSecretKey' ? meta.hasObjectStorageSecretKey : meta[k])), ...S3_SWITCHES.map(([, k]) => meta[k]), meta.chunkedUploadEnabled, meta.chunkedUploadChunkSizeMb, meta.chunkedUploadSessionTtlMinutes],
    expected: ({ data }: AdminRowContext) => {
      const d = data as Record<string, boolean>;
      return [true, ...S3_FIELDS.map(([, k, v]) => (k === 'objectStorageSecretKey' ? true : v)), ...S3_SWITCHES.map(([, k]) => d[k]), d.chunkedUploadEnabled, 7, 33];
    },
    // 秘密鍵は伏せ字で出るので、表示は見ない (設定済みかを read で見ている)
    shows: async ({ page, data }) => {
      const d = data as Record<string, boolean>;
      await expect(await switchByLabel(page, 'Use object storage')).toHaveAttribute('data-checked', 'true');
      for (const [label, k, value] of S3_FIELDS) if (k !== 'objectStorageSecretKey') await expectField(page, label, value);
      for (const [label, key] of S3_SWITCHES) await expect(await switchByLabel(page, label)).toHaveAttribute('data-checked', String(d[key]));
      const chunked = folderGroup(page, 'Enable chunked uploads');
      if ((await chunked.getAttribute('aria-expanded')) !== 'true') await chunked.getByTestId('folder-header').first().click();
      await expect(await switchIn(chunked, 'Enable chunked uploads')).toHaveAttribute('data-checked', String(d.chunkedUploadEnabled));
      await expectField(page, 'Chunk size (MiB)', '7');
      await expectField(page, 'Session lifetime (minutes)', '33');
    },
    effect: { skip: EXTERNAL('オブジェクトストレージ (S3)') },
  },
  // --- /admin/email-settings
  {
    name: 'email (SMTP)', route: '/admin/email-settings',
    keys: ['enableEmail', 'email', 'smtpHost', 'smtpPort', 'smtpUser', 'smtpPass', 'smtpSecure'],
    setup: opposites(['smtpSecure']),
    set: async ({ page, data }) => {
      await setSwitch(await switchByLabel(page, 'Enable email distribution (Recommended)'), true);
      await fillField(page, 'Email address', 'pw@remote.example');
      for (const [label, , value] of SMTP_FIELDS) await fillField(page, label, value);
      await setSwitch(await switchByLabel(page, 'Use implicit SSL/TLS for SMTP connections'), (data as Record<string, boolean>).smtpSecure);
      await savePage(page);
    },
    read: (meta) => [meta.enableEmail, meta.email, meta.smtpHost, meta.smtpPort, meta.smtpUser, meta.hasSmtpPass, meta.smtpSecure],
    expected: ({ data }: AdminRowContext) => [true, 'pw@remote.example', 'smtp.remote.example', 2525, 'pwuser', true, (data as Record<string, boolean>).smtpSecure],
    // パスワードは伏せ字で出るので、表示は見ない (設定済みかを read で見ている)
    shows: async ({ page, data }) => {
      await expect(await switchByLabel(page, 'Enable email distribution (Recommended)')).toHaveAttribute('data-checked', 'true');
      await expectField(page, 'Email address', 'pw@remote.example');
      for (const [label, k, value] of SMTP_FIELDS) if (k !== 'smtpPass') await expectField(page, label, value);
      await expect(await switchByLabel(page, 'Use implicit SSL/TLS for SMTP connections')).toHaveAttribute('data-checked', String((data as Record<string, boolean>).smtpSecure));
    },
    // 送信元と SMTP は送るときにだけ使い、SMTP サーバーが要る。有効の印は利用者に届く (登録の画面がメール欄を出す)
    effect: async ({ request }) => { expect((await expectApiOk(request, 'meta', { detail: true })).enableEmail, '/api/meta の enableEmail').toBe(true); },
  },
  // --- /admin/external-services
  {
    name: 'googleAnalyticsMeasurementId', keys: ['googleAnalyticsMeasurementId'], route: '/admin/external-services',
    set: async ({ page }) => {
      await fillField(page, 'Measurement ID', 'G-PWMETA');
      await saveFolder(page, 'Google Analytics');
    },
    read: (meta) => meta.googleAnalyticsMeasurementId,
    expected: 'G-PWMETA',
    shows: ({ page }) => expectField(page, 'Measurement ID', 'G-PWMETA'),
    // frontend はこの値を見て計測のスクリプトを読む
    effect: async ({ request }) => { expect((await expectApiOk(request, 'meta', { detail: true })).googleAnalyticsMeasurementId, '/api/meta の googleAnalyticsMeasurementId').toBe('G-PWMETA'); },
  },
  {
    name: 'DeepL (auth key, pro)', keys: ['deeplAuthKey', 'deeplIsPro'], route: '/admin/external-services',
    setup: opposites(['deeplIsPro']),
    set: async ({ page, data }) => {
      await fillField(page, 'Auth Key', 'pw-deepl-key');
      await setSwitch(await switchByLabel(page, 'Pro account'), (data as Record<string, boolean>).deeplIsPro);
      await saveFolder(page, 'DeepL Translation');
    },
    read: (meta) => [meta.hasDeeplAuthKey, meta.deeplIsPro],
    expected: ({ data }: AdminRowContext) => [true, (data as Record<string, boolean>).deeplIsPro],
    shows: async ({ page, data }) => { await expect(await switchByLabel(page, 'Pro account')).toHaveAttribute('data-checked', String((data as Record<string, boolean>).deeplIsPro)); },
    // 鍵があると翻訳を使えると利用者に知らせる。Pro かどうかは DeepL の API の宛先だけを変える
    effect: async ({ request }) => { expect((await expectApiOk(request, 'meta', { detail: true })).translatorAvailable, '/api/meta の translatorAvailable').toBe(true); },
  },
  // --- /admin/relays
  {
    name: 'relays (ephemeral notes, orphan cleanup)', route: '/admin/relays',
    keys: ['enableEphemeralRelayNotes', 'ephemeralRelayNoteTtlMinutes', 'enableRelayOrphanUserCleanup', 'relayOrphanUserGraceDays'],
    set: async ({ page }) => {
      await setSwitch(await switchIn(folderGroup(page, 'Temporary storage for relayed notes'), RELAY_EPHEMERAL), true);
      await fillField(page, 'Retention period', '77');
      await saveFolder(page, 'Temporary storage for relayed notes');
      await setSwitch(await switchIn(folderGroup(page, 'Cleaning up users from relays'), RELAY_ORPHAN), true);
      await fillField(page, 'Grace period', '9');
      await saveFolder(page, 'Cleaning up users from relays');
    },
    read: (meta) => [meta.enableEphemeralRelayNotes, meta.ephemeralRelayNoteTtlMinutes, meta.enableRelayOrphanUserCleanup, meta.relayOrphanUserGraceDays],
    expected: [true, 77, true, 9],
    shows: async ({ page }) => {
      await expect(await switchIn(folderGroup(page, 'Temporary storage for relayed notes'), RELAY_EPHEMERAL)).toHaveAttribute('data-checked', 'true');
      await expect(await switchIn(folderGroup(page, 'Cleaning up users from relays'), RELAY_ORPHAN)).toHaveAttribute('data-checked', 'true');
      await expectField(page, 'Retention period', '77');
      await expectField(page, 'Grace period', '9');
    },
    effect: { skip: REMOTE },
  },
  // --- サーバーのルール (/admin/moderation の中の枠)
  {
    name: 'serverRules', keys: ['serverRules'], route: '/admin/moderation', restore: [], restorePublic: ['serverRules'],
    set: async ({ page }) => {
      const folder = folderGroup(page, 'Server rules');
      await folder.getByRole('button', { name: 'Add' }).click();
      await folder.locator('input').last().fill('pw rule');
      await saveFolder(page, 'Server rules');
    },
    // admin/meta はサーバーのルールを返さない (本家も同じ)
    read: () => null,
    expected: null,
    shows: async ({ page }) => { await expect(folderGroup(page, 'Server rules').locator('input').last()).toHaveValue('pw rule'); },
    effect: async ({ request }) => {
      expect((await expectApiOk(request, 'meta', { detail: true })).serverRules, '/api/meta の serverRules').toContain('pw rule');
    },
  },
];

const PERF_SWITCHES: [string, string][] = [
  ['Generate remote user data charts', 'enableChartsForRemoteUser'],
  ['Receive remote server stats', 'enableStatsForFederatedInstances'],
  ['Generate remote instance data charts', 'enableChartsForFederatedInstances'],
  ['Display the role badges assigned to remote users', 'showRoleBadgesOfRemoteUsers'],
];
const FTT_FIELDS: [string, string][] = [
  ['perLocalUserUserTimelineCacheMax', '301'],
  ['perRemoteUserUserTimelineCacheMax', '102'],
  ['perUserHomeTimelineCacheMax', '303'],
  ['perUserListTimelineCacheMax', '304'],
];
const S3_FIELDS: [string, string, string][] = [
  ['Base URL', 'objectStorageBaseUrl', 'https://s3.remote.example/pw'],
  ['Bucket', 'objectStorageBucket', 'pw-bucket'],
  ['Prefix', 'objectStoragePrefix', 'pw'],
  ['Endpoint', 'objectStorageEndpoint', 's3.remote.example'],
  ['Region', 'objectStorageRegion', 'pw-region'],
  ['Access key', 'objectStorageAccessKey', 'pw-access'],
  ['Secret key', 'objectStorageSecretKey', 'pw-secret'],
];
const S3_SWITCHES: [string, string][] = [
  ['Use SSL', 'objectStorageUseSSL'],
  ['Connect over Proxy', 'objectStorageUseProxy'],
  ['Set "public-read" on upload', 'objectStorageSetPublicRead'],
  ['s3ForcePathStyle', 'objectStorageS3ForcePathStyle'],
];
const SMTP_FIELDS: [string, string, string][] = [
  ['Host', 'smtpHost', 'smtp.remote.example'],
  ['Port', 'smtpPort', '2525'],
  ['Username', 'smtpUser', 'pwuser'],
  ['Password', 'smtpPass', 'pw-pass'],
];
const RELAY_EPHEMERAL = 'Store notes from relays temporarily';
const RELAY_ORPHAN = 'Delete users nobody is connected to';

test.describe('admin: meta (security, performance, storage, email, external, relays, rules)', () => {
  test.setTimeout(90_000);
  for (const row of ROWS) {
    test(row.name, { annotation: [...coversRoutes(row.route).annotation, ...coversMeta(...row.keys).annotation] }, ({ page, request, baseURL }) => runMetaRow({ page, request, baseURL }, row));
  }
});
