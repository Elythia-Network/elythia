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
				<!-- mk-go: 戻した DB に 2FA が無いと、画面からは退避した DB へ戻せない (#3463 (6d))。 -->
				<div>{{ i18n.ts._backups.rollbackCliHint }}</div>
			</MkInfo>
			<template v-else>
				<MkSwitch v-if="hasPasskey" v-model="usePasskey">{{ i18n.ts._backups.usePasskey }}</MkSwitch>
				<div class="_buttons">
					<MkButton primary :disabled="busy" @click="load"><i class="ti ti-lock-open"></i> {{ overview ? i18n.ts._backups.reload : i18n.ts._backups.show }}</MkButton>
					<MkButton v-if="overview && overview.service.configured" :disabled="busy" @click="take"><i class="ti ti-database-export"></i> {{ i18n.ts._backups.takeNow }}</MkButton>
				</div>
			</template>

			<!--
				mk-go: 画面からのリストア (#3463 (6c))。進み具合はバックアップ用のサービスが
				持ち、メンテナンス中も ticket で読める。
			-->
			<div v-if="restore" class="_panel _gaps_s" :class="$style.restore">
				<div v-if="restore.status?.record.kind === 'rollback'" :class="$style.restoreHead"><i class="ti ti-arrow-back-up"></i> {{ i18n.ts._backups.rollbackTitle }}<span class="_monospace">: {{ restore.status.record.target }}</span></div>
				<div v-else :class="$style.restoreHead"><i class="ti ti-database-import"></i> {{ i18n.ts._backups.restoreTitle }}<span v-if="restore.status" class="_monospace">: {{ restore.status.record.generationId }}</span></div>
				<template v-if="restore.status">
					<MkInfo :warn="restore.status.record.step === 'unknown' || restore.status.record.step === 'failed'">
						{{ stepText(restore.status.record.step) }}
						<div v-if="restore.status.record.error" class="_monospace" :class="$style.verifyError">{{ restore.status.record.error }}</div>
					</MkInfo>
					<MkInfo v-if="restore.status.record.step === 'unknown'" warn>{{ i18n.ts._backups.restoreUnknownHelp }}</MkInfo>
					<MkInfo v-else-if="restore.status.maintenance">{{ i18n.ts._backups.restoreKeepOpen }}</MkInfo>
					<MkInfo v-else-if="restore.status.record.step === 'done'">{{ i18n.ts._backups.restoreSessionMayBeGone }}</MkInfo>
					<div v-if="restore.status.maintenance?.blockers?.length" :class="$style.verifyError">
						{{ i18n.ts._backups.restoreWaiting }}:
						<div v-for="b in restore.status.maintenance.blockers" :key="b.server + b.pid" class="_monospace">{{ b.server }} pid={{ b.pid }} {{ b.application }}</div>
					</div>
					<div :class="$style.journal" class="_monospace">
						<div v-for="(j, i) in restore.status.journal.slice(-12)" :key="i"><MkTime :time="j.at" mode="absolute"/> {{ j.message }}</div>
					</div>
				</template>
				<MkInfo v-if="restore.error" warn>{{ restore.error }}</MkInfo>
				<div class="_buttons">
					<MkButton v-if="restore.status?.record.step === 'ready'" primary :disabled="busy" @click="confirmRestore"><i class="ti ti-switch-horizontal"></i> {{ i18n.ts._backups.restoreSwitch }}</MkButton>
					<MkButton v-if="restore.status?.record.step === 'ready' || restore.status?.record.step === 'preparing'" danger :disabled="busy" @click="abortRestore"><i class="ti ti-x"></i> {{ i18n.ts._backups.restoreAbort }}</MkButton>
					<MkButton v-if="restore.status == null || restore.status.final" :disabled="busy" @click="closeRestore">{{ i18n.ts._backups.restoreClose }}</MkButton>
				</div>
			</div>

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
							<MkButton v-if="g.complete && g.verify?.ok && overview.service.configured && restore == null" small :disabled="busy" @click="startRestore(g)"><i class="ti ti-database-import"></i> {{ i18n.ts._backups.restore }}</MkButton>
							<MkButton small danger :disabled="busy" @click="remove(g)"><i class="ti ti-trash"></i> {{ i18n.ts.delete }}</MkButton>
						</div>
					</div>
				</MkFolder>
			</template>

			<!--
				mk-go: 退避したデータベースとロールバック (#3463 (6d))。一覧も世代の一覧と同じく
				その都度の再認証を求める。戻す処理と進み具合はリストアと同じ枠を使う。
			-->
			<div v-if="hasSecondFactor" class="_gaps_s">
				<div :class="$style.sectionHead"><i class="ti ti-arrow-back-up"></i> {{ i18n.ts._backups.keptTitle }}</div>
				<div :class="$style.caption">{{ i18n.ts._backups.keptDescription }}</div>
				<div class="_buttons">
					<MkButton :disabled="busy" @click="loadKept"><i class="ti ti-lock-open"></i> {{ kept ? i18n.ts._backups.reload : i18n.ts._backups.keptShow }}</MkButton>
				</div>
				<template v-if="kept">
					<div v-if="kept.databases.length === 0" :class="$style.caption">{{ i18n.ts._backups.keptEmpty }}</div>
					<MkFolder v-for="k in kept.databases" :key="k.name">
						<template #icon><i :class="k.problem ? 'ti ti-alert-triangle' : 'ti ti-database'"></i></template>
						<template #label><span class="_monospace">{{ k.name }}</span></template>
						<template #caption>{{ k.kind === 'rolled_back' ? i18n.ts._backups.keptRolledBack : i18n.ts._backups.keptBeforeRestore }}</template>
						<template v-if="k.size != null" #suffix>{{ bytes(k.size, 1) }}</template>
						<div class="_gaps_s">
							<MkInfo v-if="k.problem" warn>{{ i18n.ts._backups._keptProblems[k.problem] }}</MkInfo>
							<MkInfo v-else-if="k.needsMigration">{{ i18n.ts._backups.keptNeedsMigration }}</MkInfo>
							<MkKeyValue oneline>
								<template #key>{{ i18n.ts._backups.keptAt }}</template>
								<template #value><MkTime :time="k.at" mode="detail"/></template>
							</MkKeyValue>
							<MkKeyValue v-if="k.migrations != null && k.migrations.length > 0" oneline>
								<template #key>{{ i18n.ts._backups.migrations }}</template>
								<template #value><span class="_monospace">{{ k.migrations.map(m => `${m.table}=${m.missing ? '-' : m.version}${m.dirty ? ' (dirty)' : ''}`).join(', ') }}</span></template>
							</MkKeyValue>
							<div class="_buttons">
								<MkButton v-if="k.problem == null && restore == null" small :disabled="busy" @click="startRollback(k)"><i class="ti ti-arrow-back-up"></i> {{ i18n.ts._backups.rollback }}</MkButton>
							</div>
						</div>
					</MkFolder>
				</template>
			</div>
		</div>
	</div>
</PageWithHeader>
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
import MkSwitch from '@/components/MkSwitch.vue';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { createSequentialPoller } from '@/utility/sequential-poller.js';
import { rollbackCliCommand } from '@/utility/backup-rollback.js';
import type { RollbackContext } from '@/utility/backup-rollback.js';
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

function errorText(err: unknown, rollback?: RollbackContext | null): string {
	const apiErr = err as { code?: string; message?: string } | null;
	// ロールバックの照合は今のデータベースで行う (#3463 (6d))。資格情報が無くて断られた
	// ときは、コマンドでの戻し方を添える。
	const command = rollbackCliCommand(apiErr?.code, rollback);
	if (command != null) {
		return errorText(err) + '\n\n' + i18n.tsx._backups.rollbackCli({ command });
	}
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
		case 'BACKUP_SERVICE_UNREACHABLE': return i18n.ts._backups.serviceUnreachable;
		case 'GENERATION_NOT_VERIFIED': return i18n.ts._backups.restoreNotVerified;
		case 'MAINTENANCE_ACTIVE': return i18n.ts._backups.maintenanceActive;
		case 'VERSION_MISMATCH': return i18n.ts._backups.versionMismatch;
		case 'RESTORE_NOT_SUPPORTED': return i18n.ts._backups.restoreNotSupported;
		case 'RESTORE_STEP': return i18n.ts._backups.restoreStepMismatch;
		case 'RESTORE_TICKET_INVALID': return i18n.ts._backups.restoreTicketInvalid;
		case 'RESTORE_UNAVAILABLE': return i18n.ts._backups.restoreUnavailable;
		case 'NO_SUCH_KEPT_DATABASE': return i18n.ts._backups.noSuchKept;
		case 'KEPT_DATABASE_IN_USE': return i18n.ts._backups.keptInUse;
		case 'KEPT_DATABASE_TOO_NEW': return i18n.ts._backups._keptProblems.migrationTooNew;
		case 'KEPT_DATABASE_DIRTY': return i18n.ts._backups._keptProblems.migrationDirty;
		case 'KEPT_DATABASE_NOT_ELYTHIA': return i18n.ts._backups._keptProblems.notElythia;
		case 'KEPT_DATABASES_UNREADABLE': return i18n.ts._backups.keptUnreadable;
		default: return apiErr?.message ?? String(err);
	}
}

/** Runs one operation with a fresh re-authentication. */
async function run<T>(op: (auth: Elythia.BackupReauth) => Promise<T>, rollback?: RollbackContext | null): Promise<T | null> {
	if (busy.value) return null;
	busy.value = true;
	try {
		const auth = await reauth();
		if (auth == null) return null;
		return await op(auth);
	} catch (err) {
		// パスキーの取り消し (startAuthentication の reject) は黙って戻す。
		if (err instanceof Error && err.name === 'NotAllowedError') return null;
		os.alert({ type: 'error', text: errorText(err, rollback) });
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

// 進み具合を読み続けるための opId と ticket。再読み込みしても続きから読めるよう、
// このタブの sessionStorage にだけ置く (ticket だけでは何もできず、同じセッションの
// token が要る)。
const RESTORE_STORAGE_KEY = 'elythia:backupRestore';
const RESTORE_POLL_MS = 2000;

const restore = ref<{ ticket: Elythia.BackupRestoreTicket; status: Elythia.BackupRestoreStatus | null; error: string | null } | null>(null);

function saveRestoreTicket(t: Elythia.BackupRestoreTicket | null): void {
	try {
		if (t) window.sessionStorage.setItem(RESTORE_STORAGE_KEY, JSON.stringify(t));
		else window.sessionStorage.removeItem(RESTORE_STORAGE_KEY);
	} catch {
		// 保存できなくても、この画面を開いている間は読める。
	}
}

function loadRestoreTicket(): Elythia.BackupRestoreTicket | null {
	try {
		const raw = window.sessionStorage.getItem(RESTORE_STORAGE_KEY);
		if (raw == null) return null;
		const t = JSON.parse(raw) as Partial<Elythia.BackupRestoreTicket>;
		return typeof t.opId === 'string' && typeof t.ticket === 'string' ? { opId: t.opId, ticket: t.ticket } : null;
	} catch {
		return null;
	}
}

async function pollRestore(isCurrent: () => boolean): Promise<boolean> {
	const r = restore.value;
	if (r == null) return false;
	try {
		// セッションは i ではなく session で送る。本体は status を認証しないので、戻した DB に
		// このセッションが無くても読める (i で送ると、本体の認証が 401 で断る)。
		const status = await misskeyApi('admin/backup/restore/status', { ...r.ticket, session: $i.token }, null);
		if (!isCurrent()) return false;
		r.status = status;
		r.error = null;
		return !status.final;
	} catch (err) {
		if (!isCurrent()) return false;
		// メンテナンスへの出入りや本体の再起動の間は読めないことがある。読み直し続ける。
		const code = (err as { code?: string } | null)?.code;
		if (code === 'RESTORE_TICKET_INVALID' || code === 'NO_SUCH_RESTORE') {
			r.error = errorText(err);
			return false;
		}
		if (code === 'AUTHENTICATION_FAILED' || code === 'CREDENTIAL_REQUIRED') {
			// 読み直しても変わらない。サインアウトや保存した値の消去はしない (戻した DB に
			// セッションが無いだけのことがあり、サインインし直せば使える)。
			r.error = i18n.ts._backups.restoreSessionGone;
			return false;
		}
		r.error = i18n.ts._backups.restoreStatusUnavailable;
		return true;
	}
}

// 前の要求が終わってから次を予約する (間隔で回すと、応答の遅い間に要求が重なる)。
const restorePoller = createSequentialPoller(pollRestore, RESTORE_POLL_MS);

function startPolling(): void {
	restorePoller.start();
}

function stopPolling(): void {
	restorePoller.stop();
}

const kept = ref<Elythia.BackupKeptDatabases | null>(null);

async function loadKept(): Promise<void> {
	const res = await run(auth => misskeyApi('admin/backup/restore/candidates', auth));
	if (res != null) kept.value = res;
}

async function startRollback(k: Elythia.BackupKeptDatabase): Promise<void> {
	if (kept.value == null) return;
	const context = { name: k.name, database: kept.value.database };
	const { canceled } = await os.confirm({ type: 'warning', text: i18n.tsx._backups.rollbackConfirm(context) });
	if (canceled) return;
	const res = await run(auth => misskeyApi('admin/backup/restore/prepare', { ...auth, kind: 'rollback', database: k.name }), context);
	if (res == null) return;
	const ticket = { opId: res.opId, ticket: res.ticket };
	saveRestoreTicket(ticket);
	restore.value = { ticket, status: res.status, error: null };
	startPolling();
}

/** The guidance context of the rollback shown in the progress panel, if it is one. */
function rollbackContext(): RollbackContext | null {
	const rec = restore.value?.status?.record;
	return rec?.kind === 'rollback' ? { name: rec.target, database: rec.database } : null;
}

async function startRestore(g: Elythia.BackupGeneration): Promise<void> {
	const { canceled } = await os.confirm({ type: 'warning', text: i18n.tsx._backups.restoreConfirm({ id: g.id }) });
	if (canceled) return;
	const res = await run(auth => misskeyApi('admin/backup/restore/prepare', { ...auth, id: g.id }));
	if (res == null) return;
	const ticket = { opId: res.opId, ticket: res.ticket };
	saveRestoreTicket(ticket);
	restore.value = { ticket, status: res.status, error: null };
	startPolling();
}

async function confirmRestore(): Promise<void> {
	const r = restore.value;
	if (r?.status == null) return;
	const { canceled } = await os.confirm({ type: 'warning', text: i18n.tsx._backups.restoreSwitchConfirm({ before: r.status.record.before }) });
	if (canceled) return;
	await run(auth => misskeyApi('admin/backup/restore/confirm', { ...auth, ...r.ticket }), rollbackContext());
	startPolling();
}

async function abortRestore(): Promise<void> {
	const r = restore.value;
	if (r == null) return;
	// ロールバックの戻す先は、中止しても消さない。
	const text = r.status?.record.kind === 'rollback' ? i18n.ts._backups.rollbackAbortConfirm : i18n.ts._backups.restoreAbortConfirm;
	const { canceled } = await os.confirm({ type: 'warning', text });
	if (canceled) return;
	await run(auth => misskeyApi('admin/backup/restore/abort', { ...auth, ...r.ticket }), rollbackContext());
	startPolling();
}

function stepText(step: Elythia.BackupRestoreStep): string {
	return i18n.ts._backups._restoreSteps[step];
}

function closeRestore(): void {
	stopPolling();
	saveRestoreTicket(null);
	restore.value = null;
}

onMounted(() => {
	const t = loadRestoreTicket();
	if (t != null) {
		restore.value = { ticket: t, status: null, error: null };
		startPolling();
	}
});

onUnmounted(stopPolling);

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

.restore {
	padding: 16px;
}

.restoreHead {
	font-weight: bold;
}

.sectionHead {
	font-weight: bold;
}

.journal {
	font-size: 0.85em;
	opacity: 0.8;
	max-height: 240px;
	overflow: auto;
	white-space: pre-wrap;
	word-break: break-all;
}

.verifyError {
	font-size: 0.85em;
	color: var(--MI_THEME-error);
	white-space: pre-wrap;
	word-break: break-all;
}
</style>
