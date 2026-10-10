<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div ref="el" class="hiyeyicy" :class="{ wide: !narrow }">
	<div v-if="!narrow || currentPage?.route.name == null" class="nav">
		<div class="_spacer" style="--MI_SPACER-w: 700px; --MI_SPACER-min: 16px;">
			<div class="lxpfedzu _gaps">
				<div class="banner">
					<img :src="instance.iconUrl || '/favicon.ico'" alt="" class="icon"/>
				</div>

				<div class="_gaps_s">
					<MkInfo v-if="thereIsUnresolvedAbuseReport" warn>{{ i18n.ts.thereIsUnresolvedAbuseReportWarning }} <MkA to="/admin/abuses" class="_link">{{ i18n.ts.check }}</MkA></MkInfo>
					<MkInfo v-for="id in settingWarnings.visible" :key="id" warn :closable="canDismissWarnings" @close="dismissWarning(id)">{{ settingWarningDefs[id].text }} <MkA :to="settingWarningDefs[id].to" class="_link">{{ i18n.ts.configure }}</MkA></MkInfo>
					<div v-if="settingWarnings.hidden.length > 0" :class="$style.hiddenWarnings">{{ i18n.tsx._mkgoAdminWarnings.hiddenCount({ n: settingWarnings.hidden.length }) }} <button class="_textButton" @click="restoreWarnings()">{{ i18n.ts._mkgoAdminWarnings.showHidden }}</button></div>
				</div>

				<MkSuperMenu :def="menuDef" :searchIndex="searchIndex" :grid="narrow"></MkSuperMenu>
			</div>
		</div>
	</div>
	<div v-if="!(narrow && currentPage?.route.name == null)" class="main _pageContainer" style="height: 100%;">
		<NestedRouterView/>
	</div>
</div>
</template>

<script lang="ts" setup>
import { onActivated, onMounted, onUnmounted, provide, watch, ref, computed } from 'vue';
import type { SuperMenuDef } from '@/components/MkSuperMenu.vue';
import type { PageMetadata } from '@/page.js';
import { i18n } from '@/i18n.js';
import { collectPages } from '@/plugin-api.js';
import { serverPlugins } from '@/server-plugins.generated.js';

import MkSuperMenu from '@/components/MkSuperMenu.vue';
import MkInfo from '@/components/MkInfo.vue';
import { instance } from '@/instance.js';
import { $i, iAmAdmin } from '@/i.js';

// Elythia 独自の meta (#2557)。型は elythia-js の MetaDetailed が持つ (#3418)。
const approvalRequiredForSignup = instance.approvalRequiredForSignup === true;
import { lookup } from '@/utility/lookup.js';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { lookupUser, lookupUserByEmail, lookupFile } from '@/utility/admin-lookup.js';
import { classifySettingWarnings, knownSingleUserMode, lacksBotProtection, loadDismissedWarnings, updateDismissedWarnings } from '@/utility/admin-setting-warnings.js';
import type { SettingWarningId } from '@/utility/admin-setting-warnings.js';
import { isRegistrationClosed } from '@/utility/registration-mode.js';
import { definePage, provideMetadataReceiver, provideReactiveMetadata } from '@/page.js';
import { useRouter } from '@/router.js';
import { genSearchIndexes } from '@/utility/inapp-search.js';

const searchIndex = await import('search-index:admin').then(({ searchIndexes }) => genSearchIndexes(searchIndexes));

const isEmpty = (x: string | null) => x == null || x === '';

const router = useRouter();

const indexInfo = {
	title: i18n.ts.controlPanel,
	icon: 'ti ti-settings',
	hideHeader: true,
};

provide('shouldOmitHeaderTitle', false);

const INFO = ref<PageMetadata>(indexInfo);
const childInfo = ref<null | PageMetadata>(null);
const narrow = ref(false);
const view = ref(null);
const el = ref<HTMLDivElement | null>(null);
const pageProps = ref({});
const noMaintainerInformation = computed(() => isEmpty(instance.maintainerName) || isEmpty(instance.maintainerEmail));
// instance をそのまま渡す。承認制の値を別に取って上書きすると、設定を変えた直後に古い値で判定する。
const noBotProtection = computed(() => lacksBotProtection(instance));
const noEmailServer = computed(() => !instance.enableEmail);
const noInquiryUrl = computed(() => isEmpty(instance.inquiryUrl));
const thereIsUnresolvedAbuseReport = ref(false);

// 設定の警告 (#3190)。お一人様モードかどうかは公開の /api/meta に無いので admin/meta から取る
// (ページに埋め込む meta には載っているが、fetchInstance で更新されず読み込み時点の値のまま)。
// 取れるまでは出さない — 出してから消えると、お一人様サーバーで毎回ちらつく。
const settingWarningDefs: Record<SettingWarningId, { text: string; to: string }> = {
	maintainer: { text: i18n.ts.noMaintainerInformationWarning, to: '/admin/settings' },
	inquiryUrl: { text: i18n.ts.noInquiryUrlWarning, to: '/admin/settings' },
	botProtection: { text: i18n.ts.noBotProtectionWarning, to: '/admin/security' },
	emailServer: { text: i18n.ts.noEmailServerWarning, to: '/admin/email-settings' },
};
const settingWarningsLoaded = ref(false);
// null: 読めなかった。このときは閉じるボタンを出さない (保存すると既存の非表示を消しうる)。
const dismissedWarnings = ref<SettingWarningId[] | null>([]);
const settingWarnings = computed(() => {
	if (!settingWarningsLoaded.value) return { visible: [], hidden: [] };
	return classifySettingWarnings({
		active: {
			maintainer: noMaintainerInformation.value,
			inquiryUrl: noInquiryUrl.value,
			botProtection: noBotProtection.value,
			emailServer: noEmailServer.value,
		},
		// admin/meta は管理者専用なので、モデレーターには取れない。そのときはお一人様モード
		// ではないとみなす (警告が出るほうに倒す)。
		singleUserMode: knownSingleUserMode.value === true,
		dismissed: dismissedWarnings.value ?? [],
	});
});
const canDismissWarnings = computed(() => dismissedWarnings.value != null);

Promise.all([
	misskeyApi('admin/meta').then(meta => {
		knownSingleUserMode.value = meta.singleUserMode;
	}, () => {}),
	loadDismissedWarnings().then(ids => {
		dismissedWarnings.value = ids;
	}),
]).then(() => {
	settingWarningsLoaded.value = true;
});

let savingDismissedWarnings = 0;

function changeDismissedWarnings(op: (current: SettingWarningId[]) => SettingWarningId[]) {
	if (dismissedWarnings.value == null) return;
	// 押した直後に反映し、保存が終わったらサーバーの値に合わせる (別の端末の分も入る)。
	// 合わせるのは最後の保存が終わったときだけ — 途中で合わせると、後に押した分が一瞬戻る。
	dismissedWarnings.value = op(dismissedWarnings.value);
	savingDismissedWarnings++;
	updateDismissedWarnings(op).then(saved => {
		if (--savingDismissedWarnings === 0) dismissedWarnings.value = saved;
	}, async () => {
		savingDismissedWarnings--;
		os.alert({ type: 'error', text: i18n.ts._mkgoAdminWarnings.saveFailed });
		if (savingDismissedWarnings !== 0) return;
		const reloaded = await loadDismissedWarnings();
		// 読み直している間に次の保存が始まっていたら、そちらの結果に任せる (古い値で上書きしない)。
		if (savingDismissedWarnings === 0) dismissedWarnings.value = reloaded;
	});
}

function dismissWarning(id: SettingWarningId) {
	changeDismissedWarnings(current => current.includes(id) ? current : [...current, id]);
}

// 再表示するのは、いま条件を満たしていて隠れているものだけ。条件を満たしていないものまで
// 戻すと、後で設定を外したときに意図に反して出てくる。
function restoreWarnings() {
	const restoring = settingWarnings.value.hidden;
	changeDismissedWarnings(current => current.filter(id => !restoring.includes(id)));
}

const currentPage = computed(() => router.currentRef.value.child);

// IP からの関連アカウント検索 (#3104) を出せるか。既定は管理者のみで、
// 運営者がロールでモデレーターに開ける。
const canSearchIpHistory = computed(() => iAmAdmin || ($i != null && ($i.policies as unknown as Record<string, unknown>).canSearchIpHistory === true));

misskeyApi('admin/abuse-user-reports', {
	state: 'unresolved',
	limit: 1,
}).then(reports => {
	if (reports.length > 0) thereIsUnresolvedAbuseReport.value = true;
});

const NARROW_THRESHOLD = 600;
const ro = new ResizeObserver((entries, observer) => {
	if (entries.length === 0) return;
	narrow.value = entries[0].borderBoxSize[0].inlineSize < NARROW_THRESHOLD;
});

// プラグインが宣言した管理画面をメニュー項目に写す。
const pluginAdminMenu = computed(() => collectPages(serverPlugins, true).map(p => ({
	icon: p.navIcon ?? 'ti ti-puzzle',
	text: p.navTitle ?? p.plugin,
	to: `/admin${p.fullPath}`,
	active: currentPage.value?.route.name === `plugin:${p.plugin}`,
})));

const menuDef = computed<SuperMenuDef[]>(() => [{
	title: i18n.ts.quickAction,
	items: [{
		type: 'button',
		icon: 'ti ti-search',
		text: i18n.ts.lookup,
		action: adminLookup,
	}, ...(instance.disableRegistration ? [{
		type: 'button' as const,
		icon: 'ti ti-user-plus',
		text: i18n.ts.createInviteCode,
		action: invite,
	}] : [])],
}, {
	title: i18n.ts.administration,
	items: [{
		icon: 'ti ti-dashboard',
		text: i18n.ts.dashboard,
		to: '/admin/overview',
		active: currentPage.value?.route.name === 'overview',
	}, {
		icon: 'ti ti-users',
		text: i18n.ts.users,
		to: '/admin/users',
		active: currentPage.value?.route.name === 'users',
	}, {
		icon: 'ti ti-user-plus',
		text: i18n.ts.invite,
		to: '/admin/invites',
		active: currentPage.value?.route.name === 'invites',
	}, ...(approvalRequiredForSignup ? [{
		// mk-go: 承認制の登録の審査 (#2555)。**承認制のときだけ出す** — 使って
		// いないインスタンスのメニューに、押しても空の項目を並べない (#2557)。
		icon: 'ti ti-user-plus',
		text: '登録申請',
		to: '/admin/signup-applications',
		active: currentPage.value?.route.name === 'signup-applications',
	}] : []), {
		icon: 'ti ti-badges',
		text: i18n.ts.roles,
		to: '/admin/roles',
		active: currentPage.value?.route.name === 'roles',
	}, {
		icon: 'ti ti-icons',
		text: i18n.ts.customEmojis,
		to: '/admin/emojis',
		active: currentPage.value?.route.name === 'emojis',
	}, {
		icon: 'ti ti-icons',
		text: i18n.ts.customEmojis + '(beta)',
		to: '/admin/emojis2',
		active: currentPage.value?.route.name === 'emojis2',
	}, {
		icon: 'ti ti-sparkles',
		text: i18n.ts.avatarDecorations,
		to: '/admin/avatar-decorations',
		active: currentPage.value?.route.name === 'avatarDecorations',
	}, {
		icon: 'ti ti-whirl',
		text: i18n.ts.federation,
		to: '/admin/federation',
		active: currentPage.value?.route.name === 'federation',
	}, {
		icon: 'ti ti-clock-play',
		text: i18n.ts.federationJobs,
		to: '/admin/federation-job-queue',
		active: currentPage.value?.route.name === 'federationJobQueue',
	}, {
		icon: 'ti ti-clock-play',
		text: i18n.ts.jobQueue,
		to: '/admin/job-queue',
		active: currentPage.value?.route.name === 'jobQueue',
	}, {
		icon: 'ti ti-cloud',
		text: i18n.ts.files,
		to: '/admin/files',
		active: currentPage.value?.route.name === 'files',
	}, {
		icon: 'ti ti-speakerphone',
		text: i18n.ts.announcements,
		to: '/admin/announcements',
		active: currentPage.value?.route.name === 'announcements',
	}, {
		icon: 'ti ti-ad',
		text: i18n.ts.ads,
		to: '/admin/ads',
		active: currentPage.value?.route.name === 'ads',
	}, {
		icon: 'ti ti-exclamation-circle',
		text: i18n.ts.abuseReports,
		to: '/admin/abuses',
		active: currentPage.value?.route.name === 'abuses',
	}, ...(canSearchIpHistory.value ? [{
		// mk-go: IP アドレスから関連アカウントを探す (#3104)。
		//
		// **`$i.policies` だけでは足りない。** `HasRolePolicy` は管理者を短絡
		// するが `GetUserPolicies` はしないので、`policies.canSearchIpHistory` は
		// 管理者でも false のまま。それだけで絞ると、既定 (= 管理者のみ) の
		// 構成でメニューが誰にも出ない。
		icon: 'ti ti-network',
		text: i18n.ts._mkgoIpSearch.title,
		to: '/admin/ip-search',
		active: currentPage.value?.route.name === 'ip-search',
	}] : []), {
		icon: 'ti ti-list-search',
		text: i18n.ts.moderationLogs,
		to: '/admin/modlog',
		active: currentPage.value?.route.name === 'modlog',
	}],
}, {
	title: i18n.ts.settings,
	items: [{
		icon: 'ti ti-settings',
		text: i18n.ts.general,
		to: '/admin/settings',
		active: currentPage.value?.route.name === 'settings',
	}, {
		icon: 'ti ti-paint',
		text: i18n.ts.branding,
		to: '/admin/branding',
		active: currentPage.value?.route.name === 'branding',
	}, {
		icon: 'ti ti-shield',
		text: i18n.ts.moderation,
		to: '/admin/moderation',
		active: currentPage.value?.route.name === 'moderation',
	}, {
		icon: 'ti ti-mail',
		text: i18n.ts.emailServer,
		to: '/admin/email-settings',
		active: currentPage.value?.route.name === 'email-settings',
	}, {
		icon: 'ti ti-cloud',
		text: i18n.ts.objectStorage,
		to: '/admin/object-storage',
		active: currentPage.value?.route.name === 'object-storage',
	}, {
		icon: 'ti ti-lock',
		text: i18n.ts.security,
		to: '/admin/security',
		active: currentPage.value?.route.name === 'security',
	}, {
		icon: 'ti ti-planet',
		text: i18n.ts.relays,
		to: '/admin/relays',
		active: currentPage.value?.route.name === 'relays',
	}, {
		icon: 'ti ti-link',
		text: i18n.ts.externalServices,
		to: '/admin/external-services',
		active: currentPage.value?.route.name === 'external-services',
	}, {
		icon: 'ti ti-webhook',
		text: 'Webhook',
		to: '/admin/system-webhook',
		active: currentPage.value?.route.name === 'system-webhook',
	}, {
		icon: 'ti ti-bolt',
		text: i18n.ts.performance,
		to: '/admin/performance',
		active: currentPage.value?.route.name === 'performance',
	}],
}, {
	title: i18n.ts.info,
	items: [{
		icon: 'ti ti-database',
		text: i18n.ts.database,
		to: '/admin/database',
		active: currentPage.value?.route.name === 'database',
	}, ...(iAmAdmin ? [{
		// mk-go: DB のバックアップ (#3462)。管理者だけが使える (モデレーターには出さない)。
		icon: 'ti ti-database-export',
		text: i18n.ts._backups.title,
		to: '/admin/backups',
		active: currentPage.value?.route.name === 'backups',
	}] : [])],
}, {
	// サーバープラグインの節 (mk-go #2477 / #2497)。
	//
	// **ルートを生やすだけでは辿り着けない。** URL を直打ちするしかない状態に
	// なるので、メニューにも出す。先頭は一覧ページ (#2497) で、プラグインが
	// 0 個でも組み込み状況と残存データを確認できるよう常設する。続けて各
	// プラグインが宣言した管理画面を並べる。
	title: 'プラグイン',
	items: [{
		icon: 'ti ti-puzzle',
		text: 'サーバープラグイン',
		to: '/admin/server-plugins',
		active: currentPage.value?.route.name === 'server-plugins',
	}, ...pluginAdminMenu.value],
}]);

onMounted(() => {
	if (el.value != null) {
		ro.observe(el.value);
		narrow.value = el.value.offsetWidth < NARROW_THRESHOLD;
	}
	if (currentPage.value?.route.name == null && !narrow.value) {
		router.replace('/admin/overview');
	}
});

onActivated(() => {
	if (el.value != null) {
		narrow.value = el.value.offsetWidth < NARROW_THRESHOLD;
	}
	if (currentPage.value?.route.name == null && !narrow.value) {
		router.replace('/admin/overview');
	}
});

onUnmounted(() => {
	ro.disconnect();
});

watch(router.currentRef, (to) => {
	if (to.route.path === '/admin' && to.child?.route.name == null && !narrow.value) {
		router.replace('/admin/overview');
	}
});

provideMetadataReceiver((metadataGetter) => {
	const info = metadataGetter();
	if (info == null) {
		childInfo.value = null;
	} else {
		childInfo.value = info;
		INFO.value.needWideArea = info.needWideArea ?? undefined;
	}
});
provideReactiveMetadata(INFO);

function invite() {
	misskeyApi('admin/invite/create').then(x => {
		// 受け付けていない間も発行はできる。使えないことだけ添える (#3186)。
		os.alert({
			type: 'info',
			text: isRegistrationClosed() ? `${x[0].code}\n\n${i18n.ts._mkgoRegistration.inviteUnusableWhileClosed}` : x[0].code,
		});
	}).catch(err => {
		os.alert({
			type: 'error',
			text: err,
		});
	});
}

function adminLookup(ev: PointerEvent) {
	os.popupMenu([{
		text: i18n.ts.user,
		icon: 'ti ti-user',
		action: () => {
			lookupUser();
		},
	}, {
		text: `${i18n.ts.user} (${i18n.ts.email})`,
		icon: 'ti ti-user',
		action: () => {
			lookupUserByEmail();
		},
	}, {
		text: i18n.ts.file,
		icon: 'ti ti-cloud',
		action: () => {
			lookupFile();
		},
	}, {
		text: i18n.ts.lookup,
		icon: 'ti ti-world-search',
		action: () => {
			lookup();
		},
	}], ev.currentTarget ?? ev.target);
}

const headerActions = computed(() => []);

const headerTabs = computed(() => []);

definePage(() => INFO.value);
</script>

<style lang="scss" scoped>
.hiyeyicy {
	height: 100%;

	&.wide {
		display: flex;
		margin: 0 auto;

		> .nav {
			position: sticky;
			top: 0;
			width: 32%;
			max-width: 280px;
			box-sizing: border-box;
			border-right: solid 0.5px var(--MI_THEME-divider);
			overflow: auto;
			height: 100cqh;
		}

		> .main {
			flex: 1;
			min-width: 0;
		}
	}

	> .nav {
		.lxpfedzu {
			> .banner {
				margin: 16px;

				> .icon {
					display: block;
					margin: auto;
					height: 42px;
					border-radius: 8px;
				}
			}
		}
	}
}
</style>

<style lang="scss" module>
.hiddenWarnings {
	font-size: 85%;
	opacity: 0.7;
	text-align: center;
}
</style>
