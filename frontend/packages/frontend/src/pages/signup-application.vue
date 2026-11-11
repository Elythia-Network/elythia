<!--
SPDX-FileCopyrightText: Elythia-Network
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go: 承認制の登録 (#2568 / #2569)。

	申請してクレームコードを受け取り、そのコードで戻ってきて登録する。**外部の
	サーバーには一切触れない。** コードは 1 度しか表示されず、失くしたら再申請。
-->
<template>
<PageWithHeader :actions="headerActions" :tabs="headerTabs">
	<div class="_spacer" style="--MI_SPACER-w: 600px; --MI_SPACER-min: 16px; --MI_SPACER-max: 32px;">
		<div class="_gaps_m">
			<!-- mk-go: 受け付けていない間も状況の照会はできる (#3186)。申請と登録は止まる。 -->
			<MkInfo v-if="registrationClosed" warn>{{ i18n.ts._mkgoRegistration.closedNotice }}</MkInfo>
			<MkInfo v-if="fatal" warn>{{ fatal }}</MkInfo>

			<!--
				承認制でないサーバーでは、申請も照会もできない (どちらの API も 503 を返す)。
				フォームを出すと準備 (form-token) が失敗するので、案内だけを出す (#4101)。
				登録を閉じているときは上の案内と重なるので出さない。
			-->
			<template v-if="!approvalRequired">
				<MkInfo v-if="!registrationClosed" data-testid="signup-application-not-approval">{{ i18n.ts._mkgoRegistration.approvalNotEnabled }}</MkInfo>
			</template>

			<!-- 申請直後: コードを 1 度だけ見せる -->
			<template v-else-if="issuedCode">
				<MkInfo warn>
					<div><b>{{ i18n.ts._mkgoSignupApplication.saveCodeTitle }}</b>{{ i18n.ts._mkgoSignupApplication.saveCodeNotShownAgain }}</div>
					<div>{{ i18n.ts._mkgoSignupApplication.saveCodeDescription }}</div>
				</MkInfo>
				<MkInput v-model="issuedCode" readonly data-testid="signup-application-issued-code">
					<template #label>{{ i18n.ts._mkgoSignupApplication.claimCode }}</template>
				</MkInput>
				<MkButton primary rounded data-testid="signup-application-ack" @click="beginLookup">
					<i class="ti ti-check"></i> {{ i18n.ts._mkgoSignupApplication.savedCode }}
				</MkButton>
			</template>

			<!-- 確認メールを送った -->
			<template v-else-if="confirmationSent">
				<MkInfo>{{ i18n.tsx._mkgoSignupApplication.confirmationSent({ email: emailAddress }) }}</MkInfo>
				<div :class="$style.note">
					{{ i18n.ts._mkgoSignupApplication.confirmationNotArrived }}<b>{{ i18n.ts._mkgoSignupApplication.confirmationLinkInvalidated }}</b>
				</div>
				<MkButton rounded @click="confirmationSent = false">
					<i class="ti ti-arrow-back"></i> {{ i18n.ts._mkgoSignupApplication.backToRegistration }}
				</MkButton>
			</template>

			<!-- 申請の状態を表示 -->
			<template v-else-if="application">
				<template v-if="application.status === 'pending'">
					<MkInfo data-testid="signup-application-status" data-status="pending">{{ i18n.ts._mkgoSignupApplication.pending }}</MkInfo>
					<div :class="$style.note">
						{{ i18n.ts._mkgoSignupApplication.checkResultWithCode }}
						<I18n v-if="application.expiresAt" :src="i18n.ts._mkgoSignupApplication.validUntil">
							<template #time><MkTime :time="application.expiresAt" mode="detail"/></template>
						</I18n>
					</div>
					<MkButton rounded :disabled="busy" @click="refresh">
						<i class="ti ti-refresh"></i> {{ i18n.ts._mkgoSignupApplication.checkStatus }}
					</MkButton>
				</template>

				<template v-else-if="application.status === 'approved'">
					<MkInfo data-testid="signup-application-status" data-status="approved">{{ i18n.ts._mkgoSignupApplication.approved }}</MkInfo>

					<MkInput v-model="username" :disabled="busy" pattern="^[a-zA-Z0-9_]+$" data-testid="signup-application-username">
						<template #label>{{ i18n.ts.username }}</template>
						<template #prefix>@</template>
						<template v-if="usernameError" #caption>
							<span style="color: var(--MI_THEME-error)"><i class="ti ti-alert-triangle ti-fw"></i>
								<template v-if="usernameError === 'invalid-format'">{{ i18n.ts.usernameInvalidFormat }}</template>
								<template v-else-if="usernameError === 'min-range'">{{ i18n.tsx.usernameTooShort({ n: minimumUsernameLength }) }}</template>
								<template v-else>{{ i18n.ts.tooLong }}</template>
							</span>
						</template>
					</MkInput>
					<MkInput v-model="password" type="password" :disabled="busy" data-testid="signup-application-password">
						<template #label>{{ i18n.ts.password }}</template>
						<template #prefix><i class="ti ti-lock"></i></template>
					</MkInput>
					<MkInput v-if="emailRequired" v-model="emailAddress" type="email" :disabled="busy">
						<template #label>{{ i18n.ts.emailAddress }}</template>
						<template #prefix><i class="ti ti-mail"></i></template>
						<template #caption>{{ i18n.ts._mkgoSignupApplication.emailConfirmationCaption }}</template>
					</MkInput>

					<MkButton primary rounded :disabled="busy || username === '' || usernameError != null || password === '' || (emailRequired && emailAddress === '')" data-testid="signup-application-register" @click="register">
						<i class="ti ti-user-plus"></i> {{ i18n.ts.signup }}
					</MkButton>
				</template>

				<template v-else-if="application.status === 'rejected'">
					<MkInfo warn data-testid="signup-application-status" data-status="rejected">{{ i18n.ts._mkgoSignupApplication.rejected }}</MkInfo>
					<MkButton rounded @click="restart">{{ i18n.ts._mkgoSignupApplication.applyAgain }}</MkButton>
				</template>

				<template v-else-if="application.status === 'expired'">
					<MkInfo warn data-testid="signup-application-status" data-status="expired">{{ i18n.ts._mkgoSignupApplication.expired }}</MkInfo>
					<MkButton rounded @click="restart">{{ i18n.ts._mkgoSignupApplication.applyAgain }}</MkButton>
				</template>

				<template v-else-if="application.status === 'completed'">
					<MkInfo data-testid="signup-application-status" data-status="completed">{{ i18n.ts._mkgoSignupApplication.completed }}</MkInfo>
				</template>
			</template>

			<!-- 入口: 申請する / コードで戻る -->
			<template v-else>
				<div class="_gaps_s">
					<div>{{ i18n.ts._mkgoSignupApplication.approvalRequired }}</div>
					<div :class="$style.note">
						{{ i18n.ts._mkgoSignupApplication.codeShownOnce }}<b>{{ i18n.ts._mkgoSignupApplication.keepCode }}</b>
					</div>
				</div>

				<!--
					閉じている間は申請フォームを出さない (#3186)。出すとフォームの準備
					(form-token) が REGISTRATION_CLOSED で失敗し、「準備に失敗しました」と
					原因を取り違えた案内が並ぶ。状況の照会は下で続けられる。
				-->
				<MkFolder v-if="!registrationClosed" :defaultOpen="true">
					<template #icon><i class="ti ti-send"></i></template>
					<template #label>{{ i18n.ts._mkgoSignupApplication.apply }}</template>

					<div class="_gaps_m">
						<MkInfo v-if="form.length === 0">
							{{ i18n.ts._mkgoSignupApplication.noFormFields }}
						</MkInfo>
						<template v-for="(field, i) in form" :key="i">
							<MkTextarea v-if="field.type === 'textarea'" v-model="answers[i]" :disabled="busy">
								<template #label>{{ field.label }}<span v-if="field.required"> *</span></template>
							</MkTextarea>
							<MkInput v-else v-model="answers[i]" :disabled="busy">
								<template #label>{{ field.label }}<span v-if="field.required"> *</span></template>
							</MkInput>
						</template>
						<MkCaptcha v-if="instance.enableHcaptcha" ref="hcaptcha" v-model="hCaptchaResponse" provider="hcaptcha" :sitekey="instance.hcaptchaSiteKey"/>
						<MkCaptcha v-if="instance.enableMcaptcha" ref="mcaptcha" v-model="mCaptchaResponse" provider="mcaptcha" :sitekey="instance.mcaptchaSiteKey" :instanceUrl="instance.mcaptchaInstanceUrl"/>
						<MkCaptcha v-if="instance.enableRecaptcha" ref="recaptcha" v-model="reCaptchaResponse" provider="recaptcha" :sitekey="instance.recaptchaSiteKey"/>
						<MkCaptcha v-if="instance.enableTurnstile" ref="turnstile" v-model="turnstileResponse" provider="turnstile" :sitekey="instance.turnstileSiteKey"/>
						<MkCaptcha v-if="instance.enableTestcaptcha" ref="testcaptcha" v-model="testcaptchaResponse" provider="testcaptcha" :sitekey="null"/>
						<MkButton primary rounded :disabled="busy || waitingForForm || captchaIncomplete" data-testid="signup-application-apply" @click="apply">
							<i class="ti ti-send"></i> {{ i18n.ts._mkgoSignupApplication.apply }}
						</MkButton>
						<div v-if="waitingForForm" style="font-size: 0.9em; opacity: 0.8;">
							{{ i18n.ts._mkgoSignupApplication.waitBeforeSubmit }}
						</div>
						<div v-if="formTokenFailed" class="_gaps_s">
							<div style="font-size: 0.9em; opacity: 0.8;">
								{{ i18n.ts._mkgoSignupApplication.formTokenFailed }}
							</div>
							<MkButton rounded @click="fetchFormToken">
								<i class="ti ti-refresh"></i> {{ i18n.ts._mkgoSignupApplication.reload }}
							</MkButton>
						</div>
					</div>
				</MkFolder>

				<MkFolder :defaultOpen="false" data-testid="signup-application-lookup-folder">
					<template #icon><i class="ti ti-key"></i></template>
					<template #label>{{ i18n.ts._mkgoSignupApplication.lookupByCode }}</template>

					<div class="_gaps_m">
						<MkInput v-model="claimCode" :disabled="busy" data-testid="signup-application-lookup-code" @enter="refresh">
							<template #label>{{ i18n.ts._mkgoSignupApplication.claimCode }}</template>
						</MkInput>
						<MkButton rounded :disabled="busy || claimCode === ''" data-testid="signup-application-lookup" @click="refresh">
							<i class="ti ti-search"></i> {{ i18n.ts._mkgoSignupApplication.check }}
						</MkButton>
					</div>
				</MkFolder>
			</template>
		</div>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import { computed, ref, onMounted, onUnmounted, watch } from 'vue';
import type * as Elythia from 'elythia-js';
import MkButton from '@/components/MkButton.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkInput from '@/components/MkInput.vue';
import MkTextarea from '@/components/MkTextarea.vue';
import MkCaptcha from '@/components/MkCaptcha.vue';
import type { Captcha } from '@/components/MkCaptcha.vue';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { definePage } from '@/page.js';
import { i18n } from '@/i18n.js';
import { instance } from '@/instance.js';
import { login } from '@/accounts.js';
import { resolveLocalUsernameState, resolveMinimumUsernameLength } from '@/utility/local-username.js';
import { isRegistrationClosed } from '@/utility/registration-mode.js';

const claimCode = ref('');
const issuedCode = ref('');
const username = ref('');
// mk-go: この画面は承認制の登録経路で、`MkSignupDialog.form.vue` と**同じ制限**を
// 受ける (#3015)。**事前チェックが無いと理由が一切伝わらない** — サーバーは
// `signupServiceError` 経由で Fastify 形式の 400 を返すが、あの形は code を
// `message` にしか載せないので `message()` の switch には届かず、
// 「処理に失敗しました。時間をおいて試してください。」だけが出る (時間をおいても直らない)。
const minimumUsernameLength = computed<number>(() => resolveMinimumUsernameLength(instance));
const usernameError = computed(() => username.value === ''
	? null
	: resolveLocalUsernameState(username.value, minimumUsernameLength.value));
const password = ref('');
const emailAddress = ref('');
const confirmationSent = ref(false);

// メール必須なら承認済みの登録も確認メールを挟む (#2571)。
const emailRequired = computed(() => instance.emailRequiredForSignup === true);
const busy = ref(false);
const fatal = ref<string | null>(null);
const registrationClosed = isRegistrationClosed();
// Elythia 独自の meta (#2556)。型は elythia-js の MetaDetailed が持つ (#3418)。
// 端末に残った古い meta で組み上がった後に、boot の取り直しで変わりうるので追従する
const approvalRequired = computed(() => instance.approvalRequiredForSignup === true);
const application = ref<Elythia.SignupApplicationView | null>(null);
// **有効な provider のトークンを全部送る (#3037 レビュー)。**
//
// サーバーは有効な provider を**全部**検証する (upstream `SignupApiService` と
// 同じ)。1 つだけ選んで送る形にしていると、運営者が 2 つ有効にした瞬間に
// **残りが空トークンで検証され、申請が 1 件も通らなくなる**。`MkSignupDialog`
// と `MkSignin.password` は元からこの形。
const hcaptcha = ref<Captcha | undefined>();
const mcaptcha = ref<Captcha | undefined>();
const recaptcha = ref<Captcha | undefined>();
const turnstile = ref<Captcha | undefined>();
const testcaptcha = ref<Captcha | undefined>();

const hCaptchaResponse = ref<string | null>(null);
const mCaptchaResponse = ref<string | null>(null);
const reCaptchaResponse = ref<string | null>(null);
const turnstileResponse = ref<string | null>(null);
const testcaptchaResponse = ref<string | null>(null);

// captcha の実 provider が 1 つも無いときに申請を守る署名付きトークン (#2806)。
// **captcha の代替ではない** — 止まるのは「フォームを取得せずに endpoint を
// 直接叩く」bot だけ。サーバー側が要求しない構成では空文字が返るので、その
// ときは送信を抑えない。
const formToken = ref('');
const formTokenReadyAt = ref(0);
// 取得が終わるまでは送信を抑える。**未取得を「要求しない構成」と同じ扱いに
// すると**、トークンが届く前に押せてしまい、400 で終わるうえ apply の
// レート制限枠を 1 回消費する (レート制限は handler の前で数える)。
const formTokenLoaded = ref(false);
// 取得に失敗したときだけ「読み込み直す」を出す。**`!formTokenLoaded` を条件に
// すると、finally で loaded を立てる以上ボタンは一瞬しか出ない。**
const formTokenFailed = ref(false);
const now = ref(Date.now());
let ticker: number | null = null;

// 最短滞在時間が明けるまで送信ボタンを抑える。**サーバー側でも同じ判定をする** —
// ここは正規の利用者が FORM_TOKEN_TOO_SOON を見ないようにするためだけのもの。
const waitingForForm = computed(() =>
	!formTokenLoaded.value || (formToken.value !== '' && now.value < formTokenReadyAt.value));

async function fetchFormToken() {
	try {
		const res = await misskeyApi('signup-application/form-token');
		formToken.value = res.token;
		formTokenReadyAt.value = Date.now() + res.minWaitSeconds * 1000;
		formTokenFailed.value = false;
	} catch {
		// 取れなくても申請自体は試せる (要求する構成ならサーバーが弾く)。
		// ここで fatal を立てると、要求しない構成でも画面が壊れて見える。
		//
		// **送信は塞がない。** ここで塞ぐと、トークンを要求しない構成
		// (実 captcha が有効) でも、発行 endpoint が一時的に落ちただけで申請
		// 自体ができなくなる。要求する構成なら塞がなくてもサーバーが
		// FORM_TOKEN_INVALID で弾くので、塞ぐ利得は apply の枠を 1 消費しない
		// ことだけで、それより「詰まらせない」ほうを取る。
		formToken.value = '';
		formTokenReadyAt.value = 0;
		formTokenFailed.value = true;
	} finally {
		formTokenLoaded.value = true;
	}
}

// 承認制が有効になった時点で 1 度だけ取る (古い meta では無効のまま始まることがある)
watch(approvalRequired, (required) => {
	if (required && !registrationClosed && !formTokenLoaded.value) void fetchFormToken();
});

onMounted(() => {
	if (approvalRequired.value && !registrationClosed) void fetchFormToken();
	ticker = window.setInterval(() => { now.value = Date.now(); }, 500);
});

onUnmounted(() => {
	if (ticker != null) window.clearInterval(ticker);
});

// Elythia 独自の meta (#2570)。型は elythia-js の MetaDetailed が持つ (#3418)。
// 端末に残った古い meta では欠けうるので、配列でなければ空にする
const form = computed<Elythia.SignupApplicationFormField[]>(() => {
	const raw = instance.signupApplicationForm;
	return Array.isArray(raw) ? raw : [];
});

// **定義と同じ順序の値の配列を送る。** ラベルはサーバーが定義から埋めるので、
// こちらから送らない (送れると審査画面に偽のラベルを流し込める)。
const answers = ref<string[]>(form.value.map(() => ''));

// **申請フォームは誰でも叩けるので、captcha が唯一の防波堤になる** — 連絡先と
// いう自然キーが無くなり、重複申請を DB で抑止できなくなったため (#2569)。
//
// **有効な provider が 1 つでも未解答なら送信させない (#3037 レビュー 2 周目)。**
// サーバーは有効な provider を全部検証するので、未解答のまま送ると
// `CAPTCHA_FAILED` で 400 になり、1 時間 5 回の枠を 1 消費したうえに
// **解けていた側のトークンまで焼ける**。`MkSignupDialog.form.vue` と
// `MkSignin.password.vue` は元からこのゲートを持っている。
const captchaIncomplete = computed((): boolean => {
	return (instance.enableHcaptcha && !hCaptchaResponse.value) ||
		(instance.enableMcaptcha && !mCaptchaResponse.value) ||
		(instance.enableRecaptcha && !reCaptchaResponse.value) ||
		(instance.enableTurnstile && !turnstileResponse.value) ||
		(instance.enableTestcaptcha && !testcaptchaResponse.value);
});

// **失敗したらウィジェットを取り直す (#3037 レビュー 2 周目)。**
// captcha のトークンは単回使用なので、`ANSWER_REQUIRED` のように captcha を
// 消費した後で落ちる経路を踏むと、項目を直して送り直しても必ず
// `CAPTCHA_FAILED` になる。reset の口が無いと**再読み込みするまで申請が
// 通らない**。`MkSignupDialog.form.vue` の `onSignupApiError` と同じ形。
function resetCaptchas(): void {
	hcaptcha.value?.reset();
	mcaptcha.value?.reset();
	recaptcha.value?.reset();
	turnstile.value?.reset();
	testcaptcha.value?.reset();
	// **トークンも捨てる (#3037 レビュー 3 周目)。**
	// `MkCaptcha.reset()` はウィジェットを作り直すだけで `v-model` を
	// **戻さない** (戻す必要がある sitekey watcher は `callback(undefined)` を
	// 別に呼んでいる)。捨てないと `captchaIncomplete` が焼けたトークンを
	// 「解答済み」と読むので、**送信ボタンが活性のまま**になり、押すと同じ
	// トークンをもう一度送って `CAPTCHA_FAILED` + 枠を 1 消費する。
	// ゲートを足した意味が無くなる。
	hCaptchaResponse.value = null;
	mCaptchaResponse.value = null;
	reCaptchaResponse.value = null;
	turnstileResponse.value = null;
	testcaptchaResponse.value = null;
}

function captchaParams() {
	return {
		'hcaptcha-response': hCaptchaResponse.value,
		'm-captcha-response': mCaptchaResponse.value,
		'g-recaptcha-response': reCaptchaResponse.value,
		'turnstile-response': turnstileResponse.value,
		'testcaptcha-response': testcaptchaResponse.value,
	};
}

function message(err: unknown): string {
	const code = (err as { code?: string } | null)?.code;
	switch (code) {
		case 'NO_SUCH_APPLICATION': return i18n.ts._mkgoSignupApplication._errors.noSuchApplication;
		case 'NOT_APPROVED': return i18n.ts._mkgoSignupApplication._errors.notApproved;
		case 'ANSWER_REQUIRED': return i18n.ts._mkgoSignupApplication._errors.answerRequired;
		case 'ANSWER_TOO_LONG': return i18n.ts._mkgoSignupApplication._errors.answerTooLong;
		case 'FORM_CHANGED': return i18n.ts._mkgoSignupApplication._errors.formChanged;
		case 'CAPTCHA_FAILED': return i18n.ts._mkgoSignupApplication._errors.captchaFailed;
		case 'FORM_TOKEN_INVALID': return i18n.ts._mkgoSignupApplication._errors.formTokenInvalid;
		case 'FORM_TOKEN_TOO_SOON': return i18n.ts._mkgoSignupApplication._errors.formTokenTooSoon;
		case 'DENIED_USERNAME': return i18n.ts._mkgoSignupApplication._errors.unusableUsername;
		case 'EMAIL_UNAVAILABLE': return i18n.ts._mkgoSignupApplication._errors.emailUnavailable;
		case 'UNAVAILABLE': return i18n.ts._mkgoSignupApplication._errors.unavailable;
		case 'REGISTRATION_CLOSED': return i18n.ts._mkgoRegistration.closedNotice;
		case 'INVALID_USERNAME': return i18n.ts._mkgoSignupApplication._errors.unusableUsername;
		case 'USED_USERNAME': return i18n.ts._mkgoSignupApplication._errors.usedUsername;
		case 'USERNAME_TOO_SHORT': return i18n.tsx._mkgoSignupApplication._errors.usernameTooShort({ n: minimumUsernameLength.value });
		case 'DUPLICATED_USERNAME': return i18n.ts._mkgoSignupApplication._errors.usedUsername;
		case 'PASSWORD_TOO_LONG': return i18n.ts._mkgoSignupApplication._errors.passwordTooLong;
		default: return i18n.ts._mkgoSignupApplication._errors.unknown;
	}
}

async function apply() {
	busy.value = true;
	fatal.value = null;
	try {
		const res = await misskeyApi(
			'signup-application/apply',
			{ answers: answers.value, formToken: formToken.value, ...captchaParams() });
		// **コードを表示するのはここだけ。** サーバーは hash しか持っていない。
		issuedCode.value = res.claimCode;
		claimCode.value = res.claimCode;
		application.value = res.application;
	} catch (err) {
		fatal.value = message(err);
		resetCaptchas();
		// トークンが失効・使用済みなら取り直す。**そのままだと何度送っても
		// 同じエラーになる。**
		if ((err as { code?: string } | null)?.code === 'FORM_TOKEN_INVALID') {
			await fetchFormToken();
		}
	} finally {
		busy.value = false;
	}
}

// 「控えました」を押したら、控えたコードで状態表示へ移る。
function beginLookup() {
	issuedCode.value = '';
}

async function refresh() {
	if (claimCode.value === '') return;
	busy.value = true;
	fatal.value = null;
	try {
		const res = await misskeyApi(
			'signup-application/status', { claimCode: claimCode.value });
		application.value = res.application;
	} catch (err) {
		application.value = null;
		fatal.value = message(err);
	} finally {
		busy.value = false;
	}
}

function restart() {
	application.value = null;
	claimCode.value = '';
	answers.value = form.value.map(() => '');
	fatal.value = null;
}

async function register() {
	busy.value = true;
	fatal.value = null;
	try {
		const res = await misskeyApi(
			'signup-application/register',
			{
				claimCode: claimCode.value,
				username: username.value,
				password: password.value,
				federationStatus: 'pending',
				...(emailRequired.value ? { emailAddress: emailAddress.value } : {}),
			});
		// メール必須のときサーバーは 204 を返す (本体なし)。確認リンクを踏むまで
		// アカウントは作られないので、ここでログインさせない。
		if (emailRequired.value || res == null) {
			confirmationSent.value = true;
			return;
		}
		await os.alert({ type: 'success', text: i18n.ts._mkgoSignupApplication.accountCreated });
		await login(res.token);
	} catch (err) {
		fatal.value = message(err);
	} finally {
		busy.value = false;
	}
}

const headerActions = computed(() => []);

const headerTabs = computed(() => []);

definePage(() => ({
	title: i18n.ts._mkgoSignupApplication.title,
	icon: 'ti ti-user-plus',
}));
</script>

<style lang="scss" module>
.note {
	font-size: 0.9em;
	opacity: 0.8;
}
</style>
