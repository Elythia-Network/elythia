<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<MkModalWindow
	ref="dialog"
	:width="500"
	:height="600"
	@close="onClose"
	@closed="emit('closed')"
>
	<template #header>{{ i18n.ts.signup }}</template>

	<div style="overflow-x: clip;">
		<Transition
			mode="out-in"
			:enterActiveClass="$style.transition_x_enterActive"
			:leaveActiveClass="$style.transition_x_leaveActive"
			:enterFromClass="$style.transition_x_enterFrom"
			:leaveToClass="$style.transition_x_leaveTo"
		>
			<!--
				mk-go: 承認制のときは通常の登録フォームを出さない (#2556)。
				申請から始まるので、専用ページへ案内する。
			-->
			<!--
				mk-go: 受け付けていないときはフォームを出さない (#3186)。アカウントの
				切り替えメニューからも開けるので、入口のボタンを隠すだけでは足りない。
			-->
			<template v-if="registrationClosed">
				<div class="_gaps_m" style="padding: 24px;">
					<MkInfo warn>{{ i18n.ts._mkgoRegistration.closedNotice }}</MkInfo>
					<MkButton rounded @click="onClose">{{ i18n.ts.close }}</MkButton>
				</div>
			</template>
			<template v-else-if="approvalRequired">
				<div class="_gaps_m" style="padding: 24px;">
					<div>このサーバーでは、登録に申請と承認が必要です。</div>
					<div style="font-size: 0.9em; opacity: 0.8;">
						申請するとクレームコードが発行されます。結果の確認と登録に必要なので控えてください。
					</div>
					<MkButton primary rounded @click="goToApplication">
						<i class="ti ti-user-plus"></i> 登録を申請する
					</MkButton>
				</div>
			</template>
			<template v-else-if="!isAcceptedServerRule">
				<XServerRules @done="isAcceptedServerRule = true" @cancel="onClose"/>
			</template>
			<template v-else>
				<XSignup :autoSet="autoSet" @signup="onSignup" @signupEmailPending="onSignupEmailPending"/>
			</template>
		</Transition>
	</div>
</MkModalWindow>
</template>

<script lang="ts" setup>
import { useTemplateRef, ref } from 'vue';
import * as Misskey from 'misskey-js';
import XSignup from '@/components/MkSignupDialog.form.vue';
import XServerRules from '@/components/MkSignupDialog.rules.vue';
import MkModalWindow from '@/components/MkModalWindow.vue';
import MkButton from '@/components/MkButton.vue';
import MkInfo from '@/components/MkInfo.vue';
import { isRegistrationClosed } from '@/utility/registration-mode.js';
import { i18n } from '@/i18n.js';
import { instance } from '@/instance.js';
import { useRouter } from '@/router.js';

const props = withDefaults(defineProps<{
	autoSet?: boolean;
}>(), {
	autoSet: false,
});

const emit = defineEmits<{
	(ev: 'done', res: Misskey.entities.SignupResponse): void;
	(ev: 'cancelled'): void;
	(ev: 'closed'): void;
}>();

const dialog = useTemplateRef('dialog');

const isAcceptedServerRule = ref(false);

// mk-go 独自の meta なので misskey-js の型集合には無い (#2556)。
const approvalRequired = (instance as unknown as Record<string, unknown>).approvalRequiredForSignup === true;
const registrationClosed = isRegistrationClosed();

const router = useRouter();

function goToApplication() {
	onClose();
	router.push('/signup-application');
}

function onClose() {
	emit('cancelled');
	dialog.value?.close();
}

function onSignup(res: Misskey.entities.SignupResponse) {
	emit('done', res);
	dialog.value?.close();
}

function onSignupEmailPending() {
	dialog.value?.close();
}
</script>

<style lang="scss" module>
.transition_x_enterActive,
.transition_x_leaveActive {
	transition: opacity 0.3s cubic-bezier(0,0,.35,1), transform 0.3s cubic-bezier(0,0,.35,1);
}
.transition_x_enterFrom {
	opacity: 0;
	transform: translateX(50px);
}
.transition_x_leaveTo {
	opacity: 0;
	transform: translateX(-50px);
}
</style>
