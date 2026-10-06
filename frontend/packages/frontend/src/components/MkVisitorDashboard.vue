<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div v-if="instance" :class="$style.root">
	<div :class="[$style.main, $style.panel]">
		<img :src="instance.iconUrl || '/favicon.ico'" alt="" :class="$style.mainIcon"/>
		<button class="_button _acrylic" :class="$style.mainMenu" @click="showMenu"><i class="ti ti-dots"></i></button>
		<div :class="$style.mainFg">
			<h1 :class="$style.mainTitle">
				<!-- 背景色によってはロゴが見えなくなるのでとりあえず無効に -->
				<!-- <img class="logo" v-if="instance.logoImageUrl" :src="instance.logoImageUrl"><span v-else class="text">{{ instanceName }}</span> -->
				<MkA to="/">{{ instanceName }}</MkA>
			</h1>
			<div :class="$style.mainAbout">
				<!-- eslint-disable-next-line vue/no-v-html -->
				<div v-html="instance.description || i18n.ts.headlineMisskey"></div>
			</div>
			<div v-if="registrationClosed || approvalRequiredForSignup || instance.disableRegistration || instance.federation !== 'all'" :class="$style.mainWarn" class="_gaps_s">
				<!--
					mk-go: 承認制のときは招待制の文言を出さない (#2557)。招待コードは
					内部で使うだけで利用者には渡らないので、実態と食い違う。
				-->
				<!-- mk-go: 受け付けていないときは招待制と出さない (#3186)。招待コードも使えないので。 -->
				<MkInfo v-if="registrationClosed" warn>{{ i18n.ts._mkgoRegistration.closedNotice }}</MkInfo>
				<MkInfo v-else-if="approvalRequiredForSignup" warn>このサーバーの登録は承認制です。申請すると管理者が確認し、承認されると登録できます。</MkInfo>
				<MkInfo v-else-if="instance.disableRegistration" warn>{{ i18n.ts.invitationRequiredToRegister }}</MkInfo>
				<MkInfo v-if="instance.federation === 'specified'" warn>{{ i18n.ts.federationSpecified }}</MkInfo>
				<MkInfo v-else-if="instance.federation === 'none'" warn>{{ i18n.ts.federationDisabled }}</MkInfo>
			</div>
			<!-- mk-go: 「他のサーバーを探す」を削除した (#2814)。理由は docs/divergence.md の §4-2 -->
			<div class="_gaps_s" :class="$style.mainActions">
				<MkButton v-if="!registrationClosed" :class="$style.mainAction" full rounded gradate data-testid="signup" style="margin-right: 12px;" @click="signup()">{{ i18n.ts.joinThisServer }}</MkButton>
				<MkButton :class="$style.mainAction" full rounded data-testid="signin" @click="signin()">{{ i18n.ts.login }}</MkButton>
			</div>
		</div>
	</div>
	<div v-if="stats && instance.clientOptions.showActivitiesForVisitor !== false" :class="$style.stats">
		<div :class="[$style.statsItem, $style.panel]">
			<div :class="$style.statsItemLabel">{{ i18n.ts.users }}</div>
			<div :class="$style.statsItemCount"><MkNumber :value="stats.originalUsersCount"/></div>
		</div>
		<div :class="[$style.statsItem, $style.panel]">
			<div :class="$style.statsItemLabel">{{ i18n.ts.notes }}</div>
			<div :class="$style.statsItemCount"><MkNumber :value="stats.originalNotesCount"/></div>
		</div>
	</div>
	<div v-if="instance.policies.ltlAvailable && instance.clientOptions.showTimelineForVisitor !== false" :class="[$style.tl, $style.panel]">
		<div :class="$style.tlHeader">{{ i18n.ts.letsLookAtTimeline }}</div>
		<div :class="$style.tlBody">
			<MkStreamingNotesTimeline src="local"/>
		</div>
	</div>
	<div v-if="instance.clientOptions.showActivitiesForVisitor !== false" :class="$style.panel">
		<XActiveUsersChart/>
	</div>
</div>
</template>

<script lang="ts" setup>
import { ref } from 'vue';
import * as Misskey from 'misskey-js';
import { instanceName } from '@@/js/config.js';
import type { MenuItem } from '@/types/menu.js';
import XSigninDialog from '@/components/MkSigninDialog.vue';
import XSignupDialog from '@/components/MkSignupDialog.vue';
import MkButton from '@/components/MkButton.vue';
import MkStreamingNotesTimeline from '@/components/MkStreamingNotesTimeline.vue';
import MkInfo from '@/components/MkInfo.vue';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { i18n } from '@/i18n.js';
import { instance } from '@/instance.js';
import { isRegistrationClosed } from '@/utility/registration-mode.js';

// mk-go 独自の meta なので misskey-js の型集合には無い (#2557)。
const approvalRequiredForSignup = (instance as unknown as Record<string, unknown>).approvalRequiredForSignup === true;
const registrationClosed = isRegistrationClosed();
import MkNumber from '@/components/MkNumber.vue';
import XActiveUsersChart from '@/components/MkVisitorDashboard.ActiveUsersChart.vue';
import { openInstanceMenu } from '@/ui/_common_/common.js';

const stats = ref<Misskey.entities.StatsResponse | null>(null);

if (instance.clientOptions.showActivitiesForVisitor !== false) {
	misskeyApi('stats', {}).then((res) => {
		stats.value = res;
	});
}

function signin() {
	const { dispose } = os.popup(XSigninDialog, {
		autoSet: true,
	}, {
		closed: () => dispose(),
	});
}

function signup() {
	const { dispose } = os.popup(XSignupDialog, {
		autoSet: true,
	}, {
		closed: () => dispose(),
	});
}

function showMenu(ev: PointerEvent) {
	openInstanceMenu(ev);
}
</script>

<style lang="scss" module>
.root {
	position: relative;
	display: flex;
	flex-direction: column;
	gap: 16px;
	padding: 32px 0 0 0;
}

.panel {
	position: relative;
	background: var(--MI_THEME-panel);
	border-radius: var(--MI-radius);
	box-shadow: 0 12px 32px rgb(0 0 0 / 25%);
	// Elythia: エントランスでは夜空のガラスにする (utility/elythia-entrance.ts)。それ以外では変数が無いので今までどおり
	border: var(--ELYTHIA-panelBorder, none);
	-webkit-backdrop-filter: var(--ELYTHIA-panelBackdrop, none);
	backdrop-filter: var(--ELYTHIA-panelBackdrop, none);
}

.main {
	text-align: center;
}

.mainIcon {
	// Elythia: エントランスでは角を丸める (既定のアイコンは四角い夜空の絵なので)
	border-radius: var(--ELYTHIA-iconRadius, 0);
	width: 85px;
	margin-top: -47px;
	vertical-align: bottom;
	filter: drop-shadow(0 2px 5px rgba(0, 0, 0, 0.5));
}

.mainMenu {
	position: absolute;
	top: 16px;
	right: 16px;
	width: 32px;
	height: 32px;
	border-radius: 8px;
	font-size: 18px;
	z-index: 50;
}

.mainFg {
	position: relative;
	z-index: 1;
}

.mainTitle {
	display: block;
	margin: 0;
	padding: 16px 32px 24px 32px;
	font-size: 1.4em;
}

.mainLogo {
	vertical-align: bottom;
	max-height: 120px;
	max-width: min(100%, 300px);
}

.mainAbout {
	padding: 0 32px;
}

.mainWarn {
	padding: 32px 32px 0 32px;
}

.mainActions {
	padding: 32px;
}

.mainAction {
	line-height: 28px;
}

.stats {
	display: grid;
	grid-template-columns: 1fr 1fr;
	grid-gap: 16px;
}

.statsItem {
	overflow: clip;
	padding: 16px 20px;
}

.statsItemLabel {
	color: color(from var(--MI_THEME-fg) srgb r g b / 0.75);
	font-size: 0.9em;
}

.statsItemCount {
	font-weight: bold;
	font-size: 1.2em;
	color: var(--MI_THEME-accent);
}

.tl {
	overflow: clip;
}

.tlHeader {
	padding: 12px 16px;
	border-bottom: solid 1px var(--MI_THEME-divider);
}

.tlBody {
	height: 350px;
	overflow: auto;
}
</style>
