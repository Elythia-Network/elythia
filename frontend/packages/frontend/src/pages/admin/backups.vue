<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go: DB のバックアップの世代と保存先の使用量 (#3462)。純正 backend には
	この endpoint が無い。閲覧を含む全ての操作で、サーバーがその都度の再認証
	(パスワード + TOTP / バックアップコード / パスキー) を求めるので、開いただけでは
	何も読まない。
-->
<template>
<PageWithHeader>
	<div class="_spacer" style="--MI_SPACER-w: 800px; --MI_SPACER-min: 16px; --MI_SPACER-max: 32px;">
		<div class="_gaps_m">
			<div :class="$style.caption">{{ i18n.ts._backups.description }}</div>

			<MkInfo v-if="!hasSecondFactor" warn>
				{{ i18n.ts._backups.twoFactorRequired }}
				<MkA to="/settings/security" class="_link">{{ i18n.ts._backups.openSecuritySettings }}</MkA>
			</MkInfo>
			<template v-else>
				<MkSwitch v-if="hasPasskey" v-model="usePasskey">{{ i18n.ts._backups.usePasskey }}</MkSwitch>
				<div class="_buttons">
					<MkButton primary :disabled="busy" @click="load"><i class="ti ti-lock-open"></i> {{ overview ? i18n.ts._backups.reload : i18n.ts._backups.show }}</MkButton>
					<MkButton v-if="overview && overview.service.configured" :disabled="busy" @click="take"><i class="ti ti-database-export"></i> {{ i18n.ts._backups.takeNow }}</MkButton>
				</div>
			</template>

			<template v-if="overview">
				<MkInfo v-if="!overview.service.configured">{{ i18n.ts._backups.serviceNotConfigured }}</MkInfo>
				<MkInfo v-else-if="!overview.service.reachable" warn>{{ i18n.ts._backups.serviceUnreachable }}</MkInfo>
				<template v-else>
					<MkInfo v-if="overview.service.running">{{ i18n.ts._backups.serviceRunning }} ({{ overview.service.running.kind }}<template v-if="overview.service.running.generationId"> {{ overview.service.running.generationId }}</template>)</MkInfo>
					<MkInfo v-if="overview.service.overdue" warn>{{ i18n.ts._backups.overdue }}</MkInfo>
					<MkInfo v-if="overview.service.lastNotifyError" warn>{{ i18n.ts._backups.lastNotifyError }}: {{ overview.service.lastNotifyError }}</MkInfo>
				</template>

				<div class="_gaps_s">
					<MkKeyValue oneline>
						<template #key>{{ i18n.ts._backups.storage }}</template>
						<template #value>{{ overview.storageType }}</template>
					</MkKeyValue>
					<MkKeyValue oneline>
						<template #key>{{ i18n.ts._backups.totalSize }}</template>
						<template #value>{{ bytes(overview.usage.totalBytes, 1) }}</template>
					</MkKeyValue>
					<MkKeyValue oneline>
						<template #key>{{ i18n.ts._backups.generationCount }}</template>
						<template #value>{{ number(overview.usage.generationCount) }}</template>
					</MkKeyValue>
					<MkKeyValue oneline>
						<template #key>{{ i18n.ts._backups.objectCount }}</template>
						<template #value>{{ number(overview.usage.objectCount) }}</template>
					</MkKeyValue>
					<MkKeyValue v-if="overview.usage.monthlyCost != null && overview.usage.pricePerGbMonth != null" oneline>
						<template #key>{{ i18n.ts._backups.monthlyCost }}</template>
						<template #value>{{ overview.usage.monthlyCost.toFixed(2) }} ({{ i18n.tsx._backups.monthlyCostDescription({ price: overview.usage.pricePerGbMonth }) }})</template>
					</MkKeyValue>
					<MkKeyValue v-if="overview.service.configured" oneline>
						<template #key>{{ i18n.ts._backups.nextRun }}</template>
						<template #value><MkTime v-if="overview.service.nextRunAt" :time="overview.service.nextRunAt" mode="detail"/><span v-else>{{ i18n.ts._backups.none }}</span></template>
					</MkKeyValue>
					<MkKeyValue v-if="overview.service.latestUsable" oneline>
						<template #key>{{ i18n.ts._backups.latestUsable }}</template>
						<template #value><span class="_monospace">{{ overview.service.latestUsable.id }}</span></template>
					</MkKeyValue>
					<MkKeyValue v-for="r in [overview.service.lastTake, overview.service.lastVerify].filter(x => x != null)" :key="r.kind">
						<template #key>{{ r.kind === 'take' ? i18n.ts._backups.lastTake : i18n.ts._backups.lastVerify }}</template>
						<template #value>
							<i :class="r.ok ? 'ti ti-circle-check' : 'ti ti-circle-x'"></i>
							<MkTime :time="r.finishedAt" mode="detail"/>
							<span v-if="r.generationId" class="_monospace"> {{ r.generationId }}</span>
							<div v-if="!r.ok" class="_monospace" :class="$style.verifyError">{{ r.stage }}: {{ r.error }}</div>
						</template>
					</MkKeyValue>
				</div>

				<div v-if="overview.generations.length === 0" :class="$style.caption">{{ i18n.ts._backups.empty }}</div>
				<MkFolder v-for="g in overview.generations" :key="g.id">
					<template #icon><i :class="statusIcon(g)"></i></template>
					<template #label><span class="_monospace">{{ g.id }}</span></template>
					<template #caption>{{ statusText(g) }} / {{ g.encrypted ? i18n.ts._backups.encrypted : i18n.ts._backups.notEncrypted }}</template>
					<template #suffix>{{ bytes(g.size, 1) }}</template>
					<div class="_gaps_s">
						<MkInfo v-if="!g.complete" warn>{{ i18n.ts._backups.incompleteDescription }}<template v-if="g.metaError"> ({{ g.metaError }})</template></MkInfo>
						<MkKeyValue oneline>
							<template #key>{{ i18n.ts._backups.createdAt }}</template>
							<template #value><MkTime :time="g.createdAt" mode="detail"/></template>
						</MkKeyValue>
						<MkKeyValue oneline>
							<template #key>{{ i18n.ts._backups.size }}</template>
							<template #value>{{ bytes(g.size, 1) }}</template>
						</MkKeyValue>
						<MkKeyValue v-if="g.elythiaVersion" oneline>
							<template #key>{{ i18n.ts._backups.elythiaVersion }}</template>
							<template #value>{{ g.elythiaVersion }}<span v-if="g.elythiaCommit" class="_monospace"> ({{ g.elythiaCommit.slice(0, 8) }})</span></template>
						</MkKeyValue>
						<MkKeyValue v-if="g.postgresVersion" oneline>
							<template #key>{{ i18n.ts._backups.postgresVersion }}</template>
							<template #value>{{ g.postgresVersion }}</template>
						</MkKeyValue>
						<MkKeyValue v-if="g.migrations.length > 0" oneline>
							<template #key>{{ i18n.ts._backups.migrations }}</template>
							<template #value><span class="_monospace">{{ g.migrations.map(m => `${m.table}=${m.missing ? '-' : m.version}${m.dirty ? ' (dirty)' : ''}`).join(', ') }}</span></template>
						</MkKeyValue>
						<MkKeyValue v-if="g.verify" oneline>
							<template #key>{{ i18n.ts._backups.verifiedAt }}</template>
							<template #value><MkTime v-if="g.verify.verifiedAt" :time="g.verify.verifiedAt" mode="detail"/></template>
						</MkKeyValue>
						<div v-if="g.verify && !g.verify.ok" class="_monospace" :class="$style.verifyError">
							<div v-if="g.verify.error">{{ g.verify.error }}</div>
							<div v-for="s in g.verify.stages.filter(x => !x.ok)" :key="s.stage">{{ s.stage }}: {{ s.skipped ? 'skipped' : s.error }}</div>
							<div v-for="m in g.verify.mismatches" :key="m.table">{{ m.table }}: {{ m.expected }} → {{ m.actual }}</div>
						</div>
						<div class="_buttons">
							<MkButton v-if="g.complete && overview.service.configured" small :disabled="busy" @click="verify(g)"><i class="ti ti-checkup-list"></i> {{ i18n.ts._backups.verifyNow }}</MkButton>
							<MkButton v-if="g.complete" small :disabled="busy" @click="download(g)"><i class="ti ti-download"></i> {{ i18n.ts._backups.download }}</MkButton>
							<MkButton small danger :disabled="busy" @click="remove(g)"><i class="ti ti-trash"></i> {{ i18n.ts.delete }}</MkButton>
						</div>
					</div>
				</MkFolder>
			</template>
		</div>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import { computed, ref } from 'vue';
import { startAuthentication } from '@simplewebauthn/browser';
import type * as Elythia from 'elythia-js';
import type { PublicKeyCredentialRequestOptionsJSON } from '@simplewebauthn/browser';
import MkButton from '@/components/MkButton.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkSwitch from '@/components/MkSwitch.vue';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { i18n } from '@/i18n.js';
import { definePage } from '@/page.js';
import { ensureSignin } from '@/i.js';
import bytes from '@/filters/bytes.js';
import number from '@/filters/number.js';

const $i = ensureSignin();

// サーバーは TOTP かパスキーの登録を求める (パスキーは TOTP を有効にしてからでないと
// 登録できないので、実質は TOTP の有無)。未登録なら操作のボタンを出さずに案内する。
const hasPasskey = computed(() => ($i.securityKeysList?.length ?? 0) > 0);
const hasSecondFactor = computed(() => $i.twoFactorEnabled || hasPasskey.value);
// TOTP を外してパスキーだけが残っているアカウント (TS 版から引き継いだもの) は、
// コードを入れる欄の無いダイアログでは再認証できないので、パスキーを既定にする。
const usePasskey = ref(!$i.twoFactorEnabled && hasPasskey.value);

const overview = ref<Elythia.BackupOverview | null>(null);
const busy = ref(false);

// サーバーの署名付き URL / ダウンロード用 URL の期限 (backupadmin.DefaultDownloadTTL)。
const DOWNLOAD_TTL_MINUTES = 5;

/**
 * Asks for the re-authentication the server requires on every operation.
 * Returns null when cancelled.
 */
async function reauth(): Promise<Elythia.BackupReauth | null> {
	if (usePasskey.value && hasPasskey.value) {
		// パスワードを先に聞く。challenge の期限があるので、パスキーは最後に出す。
		const pw = await os.inputText({ type: 'password', title: i18n.ts.password });
		if (pw.canceled || pw.result == null || pw.result === '') return null;
		const options = await misskeyApi('admin/backup/reauth-challenge', {});
		const credential = await startAuthentication({ optionsJSON: options as unknown as PublicKeyCredentialRequestOptionsJSON });
		return { password: pw.result, credential: credential as unknown as Record<string, unknown> };
	}
	const auth = await os.authenticateDialog();
	if (auth.canceled) return null;
	return { password: auth.result.password, token: auth.result.token ?? undefined };
}

function errorText(err: unknown): string {
	const apiErr = err as { code?: string; message?: string } | null;
	switch (apiErr?.code) {
		case 'TWO_FACTOR_REQUIRED': return i18n.ts._backups.twoFactorRequired;
		case 'REAUTHENTICATION_FAILED': return i18n.ts._backups.reauthFailed;
		case 'REAUTHENTICATION_REQUIRED': return i18n.ts._backups.reauthRequired;
		case 'TWO_FACTOR_CODE_ALREADY_USED': return i18n.ts._backups.codeAlreadyUsed;
		case 'PASSKEY_UNAVAILABLE': return i18n.ts._backups.passkeyUnavailable;
		case 'RATE_LIMIT_EXCEEDED': return i18n.ts._backups.rateLimited;
		case 'BACKUP_NOT_CONFIGURED': return i18n.ts._backups.notConfigured;
		case 'BACKUP_SERVICE_NOT_CONFIGURED': return i18n.ts._backups.serviceNotConfigured;
		case 'BACKUP_SERVICE_BUSY': return i18n.ts._backups.serviceBusy;
		default: return apiErr?.message ?? String(err);
	}
}

/** Runs one operation with a fresh re-authentication. */
async function run<T>(op: (auth: Elythia.BackupReauth) => Promise<T>): Promise<T | null> {
	if (busy.value) return null;
	busy.value = true;
	try {
		const auth = await reauth();
		if (auth == null) return null;
		return await op(auth);
	} catch (err) {
		// パスキーの取り消し (startAuthentication の reject) は黙って戻す。
		if (err instanceof Error && err.name === 'NotAllowedError') return null;
		os.alert({ type: 'error', text: errorText(err) });
		return null;
	} finally {
		busy.value = false;
	}
}

async function load(): Promise<void> {
	const res = await run(auth => misskeyApi('admin/backup/list', auth));
	if (res != null) overview.value = res;
}

async function take(): Promise<void> {
	const { canceled } = await os.confirm({ type: 'question', text: i18n.ts._backups.takeConfirm });
	if (canceled) return;
	const res = await run(auth => misskeyApi('admin/backup/take', auth));
	if (res != null) os.alert({ type: 'success', text: i18n.ts._backups.takeAccepted });
}

async function verify(g: Elythia.BackupGeneration): Promise<void> {
	const res = await run(auth => misskeyApi('admin/backup/verify', { ...auth, id: g.id }));
	if (res != null) os.alert({ type: 'success', text: i18n.ts._backups.verifyAccepted });
}

async function download(g: Elythia.BackupGeneration): Promise<void> {
	const res = await run(auth => misskeyApi('admin/backup/download', { ...auth, id: g.id }));
	if (res == null) return;
	// 署名付き URL もサーバーの URL も Content-Disposition: attachment で返るので、
	// 画面を離れずにダウンロードが始まる。
	const a = window.document.createElement('a');
	a.href = res.url;
	a.download = res.fileName;
	a.rel = 'noopener noreferrer';
	a.click();
	let text = i18n.tsx._backups.downloadDescription({ minutes: DOWNLOAD_TTL_MINUTES });
	if (res.encrypted) text += '\n' + i18n.ts._backups.encryptedDownload;
	os.alert({ type: 'info', text });
}

async function remove(g: Elythia.BackupGeneration): Promise<void> {
	const { canceled } = await os.confirm({ type: 'warning', text: i18n.tsx._backups.deleteConfirm({ id: g.id }) });
	if (canceled) return;
	const res = await run(auth => misskeyApi('admin/backup/delete', { ...auth, id: g.id }));
	if (res == null) return;
	// 一覧を読み直すにはもう一度再認証が要るので、消した世代だけ手元で外す。
	if (overview.value) {
		overview.value.generations = overview.value.generations.filter(x => x.id !== g.id);
		overview.value.usage.totalBytes -= res.freedBytes;
		overview.value.usage.generationCount = overview.value.generations.length;
	}
	os.alert({ type: 'success', text: i18n.tsx._backups.deleted({ size: bytes(res.freedBytes, 1) }) });
}

function statusIcon(g: Elythia.BackupGeneration): string {
	if (!g.complete) return 'ti ti-alert-triangle';
	if (g.verify == null) return 'ti ti-help-circle';
	return g.verify.ok ? 'ti ti-circle-check' : 'ti ti-circle-x';
}

function statusText(g: Elythia.BackupGeneration): string {
	if (!g.complete) return i18n.ts._backups.incomplete;
	if (g.verify == null) return i18n.ts._backups.notVerified;
	return g.verify.ok ? i18n.ts._backups.verified : i18n.ts._backups.verifyFailed;
}

definePage(() => ({
	title: i18n.ts._backups.title,
	icon: 'ti ti-database-export',
}));
</script>

<style lang="scss" module>
.caption {
	font-size: 0.9em;
	opacity: 0.8;
}

.verifyError {
	font-size: 0.85em;
	color: var(--MI_THEME-error);
	white-space: pre-wrap;
	word-break: break-all;
}
</style>
