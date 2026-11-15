<!--
SPDX-FileCopyrightText: Elythia-Network
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<MkFolder v-if="!unsupported">
	<template #icon><i class="ti ti-transfer"></i></template>
	<template #label>{{ i18n.ts._objectStorageMigration.title }}</template>
	<template #suffix>{{ runningLabel }}</template>

	<div class="_gaps_m" data-testid="object-storage-migration">
		<div>{{ i18n.ts._objectStorageMigration.description }}</div>

		<MkInfo v-if="status != null && !status.objectStorageEnabled" warn>{{ i18n.ts._objectStorageMigration.notConfigured }}</MkInfo>
		<MkInfo v-if="status != null && status.running != null">{{ i18n.ts._objectStorageMigration.running }}</MkInfo>

		<div v-if="status != null" class="_gaps_s">
			<MkKeyValue data-testid="object-storage-migration-local">
				<template #key>{{ i18n.ts._objectStorageMigration.localFiles }}</template>
				<template #value>{{ number(status.localFiles) }}</template>
			</MkKeyValue>
			<MkKeyValue data-testid="object-storage-migration-migrated">
				<template #key>{{ i18n.ts._objectStorageMigration.migratedFiles }}</template>
				<template #value>{{ number(status.migratedFiles) }}</template>
			</MkKeyValue>
			<MkKeyValue>
				<template #key>{{ i18n.ts._objectStorageMigration.verifiedFiles }}</template>
				<template #value>{{ number(status.verifiedFiles) }}</template>
			</MkKeyValue>
			<MkKeyValue>
				<template #key>{{ i18n.ts._objectStorageMigration.purgeableFiles }}</template>
				<template #value>{{ number(status.purgeableFiles) }}</template>
			</MkKeyValue>
			<MkKeyValue>
				<template #key>{{ i18n.ts._objectStorageMigration.localDeletedFiles }}</template>
				<template #value>{{ number(status.localDeletedFiles) }}</template>
			</MkKeyValue>
		</div>

		<div class="_buttons">
			<MkButton data-testid="object-storage-migration-migrate" :disabled="!canMigrate" @click="migrate"><i class="ti ti-cloud-upload"></i> {{ i18n.ts._objectStorageMigration.migrate }}</MkButton>
			<MkButton data-testid="object-storage-migration-verify" :disabled="!canVerify" @click="verify"><i class="ti ti-checkup-list"></i> {{ i18n.ts._objectStorageMigration.verify }}</MkButton>
			<MkButton data-testid="object-storage-migration-purge" danger :disabled="!canPurge" @click="purge"><i class="ti ti-trash"></i> {{ i18n.ts._objectStorageMigration.purge }}</MkButton>
			<MkButton data-testid="object-storage-migration-restore" :disabled="!canRestore" @click="restore"><i class="ti ti-cloud-download"></i> {{ i18n.ts._objectStorageMigration.restore }}</MkButton>
		</div>
		<MkInfo v-if="status != null && status.purgeableFiles > 0 && !hasSecondFactor">{{ i18n.ts._objectStorageMigration.purgeNeedsTwoFactor }}</MkInfo>

		<template v-for="list in failureLists" :key="list.key">
			<MkFolder v-if="list.count > 0">
				<template #label>{{ list.label }}</template>
				<template #suffix>{{ number(list.count) }}</template>
				<div class="_gaps_s">
					<div v-for="f in list.failures" :key="f.fileId" :class="$style.failure">
						<span class="_monospace">{{ f.fileId }}</span>
						<span>{{ f.error }}</span>
						<MkTime :time="f.failedAt"/>
					</div>
				</div>
			</MkFolder>
		</template>
	</div>
</MkFolder>
</template>

<script lang="ts" setup>
import { computed, onMounted, onUnmounted, ref } from 'vue';
import { startAuthentication } from '@simplewebauthn/browser';
import type * as Elythia from 'elythia-js';
import type { PublicKeyCredentialRequestOptionsJSON } from '@simplewebauthn/browser';
import MkButton from '@/components/MkButton.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkTime from '@/components/global/MkTime.vue';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { createSequentialPoller } from '@/utility/sequential-poller.js';
import { i18n } from '@/i18n.js';
import { ensureSignin } from '@/i.js';
import number from '@/filters/number.js';

const $i = ensureSignin();

// 実行中だけ取り直す (#4184)。件数はサーバーが drive_file を全件読んで数えるので、
// 実行中でないときは開いたときと操作の後だけ取る。
const POLL_MS = 10_000;

// ローカルを消す処理は、DB のバックアップと同じ再認証を求める (二段階認証かパスキーの
// 登録が要る)。パスキーは TOTP を有効にしてからでないと登録できないが、TS 版から
// 引き継いだ DB では TOTP だけが外れた行もあるので、パスキーだけの場合も扱う。
const hasPasskey = computed(() => ($i.securityKeysList?.length ?? 0) > 0);
const hasSecondFactor = computed(() => $i.twoFactorEnabled || hasPasskey.value);

const status = ref<Elythia.DriveObjectStorageMigrationStatus | null>(null);
const busy = ref(false);
// 本家の backend には移行の API が無い。無いと分かったときだけ部品ごと隠す。
const unsupported = ref(false);
// 画面を離れた後に、await から戻った処理が poller を動かさないようにする。
let disposed = false;

/** The status while no operation runs and none is being started, or null. */
const idle = computed(() => {
	const s = status.value;
	return s != null && s.objectStorageEnabled && s.running == null && !busy.value ? s : null;
});
const canMigrate = computed(() => (idle.value?.localFiles ?? 0) > 0);
const canVerify = computed(() => (idle.value?.migratedFiles ?? 0) > 0);
const canPurge = computed(() => (idle.value?.purgeableFiles ?? 0) > 0 && hasSecondFactor.value);
const canRestore = computed(() => (idle.value?.migratedFiles ?? 0) > 0);

const runningLabel = computed(() => {
	switch (status.value?.running) {
		case 'migrate': return i18n.ts._objectStorageMigration.migrating;
		case 'verify': return i18n.ts._objectStorageMigration.verifying;
		case 'purge': return i18n.ts._objectStorageMigration.purging;
		case 'restore': return i18n.ts._objectStorageMigration.restoring;
		default: return '';
	}
});

const failureLists = computed(() => {
	const s = status.value;
	if (s == null) return [];
	return [
		{ key: 'migrate', label: i18n.ts._objectStorageMigration.migrateFailures, count: s.failedFiles, failures: s.failures },
		{ key: 'verify', label: i18n.ts._objectStorageMigration.verifyFailures, count: s.verifyFailedFiles, failures: s.verifyFailures },
		{ key: 'purge', label: i18n.ts._objectStorageMigration.purgeFailures, count: s.purgeFailedFiles, failures: s.purgeFailures },
		{ key: 'restore', label: i18n.ts._objectStorageMigration.restoreFailures, count: s.restoreFailedFiles, failures: s.restoreFailures },
	];
});

async function load(): Promise<void> {
	status.value = await misskeyApi('admin/drive/object-storage-migration/status', {});
}

const poller = createSequentialPoller(async (isCurrent) => {
	const res = await misskeyApi('admin/drive/object-storage-migration/status', {});
	if (!isCurrent()) return false;
	status.value = res;
	return res.running != null;
}, POLL_MS);

/** Tells the error of a request to the admin. */
function alertError(err: unknown): void {
	// パスキーの取り消し (startAuthentication の reject) は黙って戻す。
	if (err instanceof Error && err.name === 'NotAllowedError') return;
	os.alert({ type: 'error', text: errorText(err) });
}

function errorText(err: unknown): string {
	const apiErr = err as { code?: string; message?: string } | null;
	switch (apiErr?.code) {
		case 'OBJECT_STORAGE_NOT_CONFIGURED': return i18n.ts._objectStorageMigration.notConfigured;
		case 'NO_LOCAL_FILES': return i18n.ts._objectStorageMigration.noLocalFiles;
		case 'NO_MIGRATED_FILES': return i18n.ts._objectStorageMigration.noMigratedFiles;
		case 'NO_PURGEABLE_FILES': return i18n.ts._objectStorageMigration.noPurgeableFiles;
		case 'STORAGE_MIGRATION_RUNNING': return i18n.ts._objectStorageMigration.alreadyRunning;
		case 'JOB_QUEUE_UNAVAILABLE': return i18n.ts._objectStorageMigration.queueUnavailable;
		case 'TWO_FACTOR_REQUIRED': return i18n.ts._objectStorageMigration.purgeNeedsTwoFactor;
		case 'REAUTHENTICATION_FAILED': return i18n.ts._backups.reauthFailed;
		case 'REAUTHENTICATION_REQUIRED': return i18n.ts._backups.reauthRequired;
		case 'TWO_FACTOR_CODE_ALREADY_USED': return i18n.ts._backups.codeAlreadyUsed;
		case 'PASSKEY_UNAVAILABLE': return i18n.ts._backups.passkeyUnavailable;
		case 'RATE_LIMIT_EXCEEDED': return i18n.ts._backups.rateLimited;
		case 'PASSWORD_NOT_SET': return i18n.ts._objectStorageMigration.passwordNotSet;
		case 'AUTHENTICATION_UNAVAILABLE': return i18n.ts._objectStorageMigration.authenticationUnavailable;
		default: return apiErr?.message ?? String(err);
	}
}

/** Starts polling when an operation runs, unless the page was left. */
function pollIfRunning(): void {
	if (disposed || status.value?.running == null) return;
	poller.start();
}

/** Runs one start request, then reads the status again and polls while it runs. */
async function run(op: () => Promise<unknown>): Promise<void> {
	if (busy.value) return;
	busy.value = true;
	let failedToLoad = false;
	try {
		try {
			await op();
		} catch (err) {
			alertError(err);
		}
		poller.stop();
		// 読み直すまで busy を下ろさない。古い status (running が null) のままボタンが押せる
		// 間に別の処理を始めると、サーバーが 409 で断る (ローカルを消す処理では、再認証の
		// コードを使い切ってから)。
		try {
			await load();
		} catch {
			// 開始前の status を残すと、始めた直後でもボタンが押せる。捨ててボタンを止め、取れる
			// まで poller で読み直す。
			status.value = null;
			failedToLoad = true;
		}
	} finally {
		busy.value = false;
	}
	if (failedToLoad) {
		if (!disposed) poller.start();
		return;
	}
	pollIfRunning();
}

/**
 * Asks for the re-authentication of admin/backup/*. Returns null when
 * cancelled.
 */
async function reauth(): Promise<Elythia.BackupReauth | null> {
	if (!$i.twoFactorEnabled && hasPasskey.value) {
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

async function migrate(): Promise<void> {
	const { canceled } = await os.confirm({ type: 'warning', text: i18n.ts._objectStorageMigration.migrateConfirm });
	if (canceled) return;
	await run(() => misskeyApi('admin/drive/object-storage-migration/start', {}));
}

async function verify(): Promise<void> {
	const { canceled } = await os.confirm({ type: 'question', text: i18n.ts._objectStorageMigration.verifyConfirm });
	if (canceled) return;
	await run(() => misskeyApi('admin/drive/object-storage-migration/verify', {}));
}

async function purge(): Promise<void> {
	const { canceled } = await os.confirm({ type: 'warning', text: i18n.ts._objectStorageMigration.purgeConfirm });
	if (canceled) return;
	// 再認証は run の外で聞く。os.authenticateDialog は閉じるボタンで閉じると resolve しない
	// ので、run の中で待つと busy が立ったままになり、再読み込みするまでボタンが押せなくなる。
	let auth: Elythia.BackupReauth | null;
	try {
		auth = await reauth();
	} catch (err) {
		alertError(err);
		return;
	}
	if (auth == null) return;
	const body = auth;
	await run(() => misskeyApi('admin/drive/object-storage-migration/purge-local', body));
}

async function restore(): Promise<void> {
	const { canceled } = await os.confirm({ type: 'question', text: i18n.ts._objectStorageMigration.restoreConfirm });
	if (canceled) return;
	await run(() => misskeyApi('admin/drive/object-storage-migration/restore', {}));
}

/**
 * Tells whether err says the backend has no such endpoint. The upstream
 * backend answers a POST to an unknown endpoint with the 404 of fastify,
 * whose `error` is a string, and a GET with UNKNOWN_API_ENDPOINT.
 */
function isUnknownEndpoint(err: unknown): boolean {
	// fetch の失敗 (TypeError)、HTML の 502 (SyntaxError)、error の無い本文 (undefined) は
	// 一時的な失敗として扱い、隠さない。
	if (typeof err === 'string') return true;
	return err != null && typeof err === 'object' && (err as { code?: unknown }).code === 'UNKNOWN_API_ENDPOINT';
}

onMounted(async () => {
	try {
		await load();
	} catch (err) {
		if (isUnknownEndpoint(err)) {
			unsupported.value = true;
			return;
		}
		// 一時的な失敗 (lock の読み出しなど) で部品を消すと、再読み込みするまで戻らない。
		// poller で取り直す (poller は失敗しても次を予約する)。
		if (!disposed) poller.start();
		return;
	}
	pollIfRunning();
});

onUnmounted(() => {
	disposed = true;
	poller.stop();
});
</script>

<style lang="scss" module>
.failure {
	display: flex;
	flex-wrap: wrap;
	gap: 0 8px;
	font-size: 0.9em;
	word-break: break-all;
}
</style>
