<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<PageWithHeader :tabs="headerTabs">
	<div class="_spacer" style="--MI_SPACER-w: 700px; --MI_SPACER-min: 16px; --MI_SPACER-max: 32px;">
		<SearchMarker path="/admin/moderation" :label="i18n.ts.moderation" :keywords="['moderation']" icon="ti ti-shield" :inlining="['serverRules']">
			<div class="_gaps_m">
				<!--
					mk-go: 登録の受け付け方を 4 択にする (#3186)。本家は「アカウント作成を
					許可」のスイッチ 1 つで、mk-go は承認制 (#2557) と「受け付けない」を
					足している。スイッチを重ねると組み合わせの整合 (#2565 / #2803) を
					管理者に考えさせることになるので、選んだ受け付け方をそのまま送る。
				-->
				<!--
					どれにも当てはまらない (null) ときは何も選ばれていない表示になる。
					**このコメントを SearchMarker の中の先頭に置かない。** 本番ビルドでは
					コメントが AST から落ち、検索索引のプラグインが「最初の子の直前の >」を
					開始タグの終わりとみなすので、コメントの閉じ記号の中に属性を書き込んでビルドが壊れる。
				-->
				<SearchMarker :keywords="['open', 'registration', 'invite', 'approval', 'closed']">
					<MkRadios :modelValue="registrationMode as RegistrationMode" :options="registrationModeDef" vertical @update:modelValue="onChange_registrationMode">
						<template #label><SearchLabel>{{ i18n.ts._mkgoRegistration.mode }}</SearchLabel></template>
						<template #caption>
							<div v-if="registrationMode == null"><i class="ti ti-alert-triangle" style="color: var(--MI_THEME-warn);"></i> {{ i18n.ts._mkgoRegistration.inconsistent }}</div>
							<div v-if="registrationMode === 'open'"><SearchText>{{ i18n.ts._serverSettings.thisSettingWillAutomaticallyOffWhenModeratorsInactive }}</SearchText></div>
							<div v-if="registrationMode === 'open'"><i class="ti ti-alert-triangle" style="color: var(--MI_THEME-warn);"></i> <SearchText>{{ i18n.ts._serverSettings.openRegistrationWarning }}</SearchText></div>
							<template v-if="registrationMode === 'approval'">
								<div v-if="emailRequiredForSignup">{{ i18n.ts._mkgoRegistration.approvalEmailNote }}</div>
								<div><MkA to="/admin/signup-applications" class="_link">{{ i18n.ts._mkgoRegistration.approvalListNote }}</MkA></div>
							</template>
						</template>
					</MkRadios>
				</SearchMarker>

				<!--
					mk-go: 申請フォームの項目 (#2570)。fediverse アカウントの欄など、
					聞きたいことを管理者が決める。**検証はしない** — 単なる自由記述。
				-->
				<!-- 閉じている間も承認制の値は残るので、再開前に項目を直せるようにする。 -->
				<MkFolder v-if="approvalEnabled" :defaultOpen="false">
					<template #icon><i class="ti ti-forms"></i></template>
					<template #label>申請フォームの項目</template>
					<template #suffix>{{ signupApplicationForm.length }} 項目</template>

					<div class="_gaps_m">
						<div v-if="signupApplicationForm.length === 0" style="font-size: 0.9em; opacity: 0.8;">
							項目を追加しないと、申請者は理由を書かずに申請することになります。
						</div>

						<div v-for="(field, i) in signupApplicationForm" :key="i" class="_gaps_s" :class="$style.formField">
							<MkInput v-model="field.label">
								<template #label>項目名</template>
							</MkInput>
							<MkSelect v-model="field.type" :items="fieldTypeDef">
								<template #label>入力欄</template>
							</MkSelect>
							<MkSwitch v-model="field.required">
								<template #label>必須にする</template>
							</MkSwitch>
							<MkButton danger inline @click="removeField(i)">
								<i class="ti ti-trash"></i> この項目を削除
							</MkButton>
						</div>

						<MkButton :disabled="signupApplicationForm.length >= maxFormFields" @click="addField">
							<i class="ti ti-plus"></i> 項目を追加
						</MkButton>
						<div v-if="signupApplicationForm.length >= maxFormFields" style="font-size: 0.9em; opacity: 0.8;">
							項目は{{ maxFormFields }}個までです。
						</div>

						<MkButton primary @click="saveSignupApplicationForm">
							<i class="ti ti-device-floppy"></i> 保存
						</MkButton>
					</div>
				</MkFolder>

				<SearchMarker :keywords="['email', 'required', 'signup']">
					<MkSwitch v-model="emailRequiredForSignup" @change="onChange_emailRequiredForSignup">
						<template #label><SearchLabel>{{ i18n.ts.emailRequiredForSignup }}</SearchLabel> ({{ i18n.ts.recommended }})</template>
						<template v-if="registrationMode === 'approval'" #caption>
							承認済みの登録にも確認メールを挟みます (#2571)。
						</template>
					</MkSwitch>
				</SearchMarker>

				<SearchMarker :keywords="['ugc', 'content', 'visibility', 'visitor', 'guest']">
					<MkSelect v-model="ugcVisibilityForVisitor" :items="ugcVisibilityForVisitorDef" @update:modelValue="onChange_ugcVisibilityForVisitor">
						<template #label><SearchLabel>{{ i18n.ts._serverSettings.userGeneratedContentsVisibilityForVisitor }}</SearchLabel></template>
						<template #caption>
							<div><SearchText>{{ i18n.ts._serverSettings.userGeneratedContentsVisibilityForVisitor_description }}</SearchText></div>
							<div><i class="ti ti-alert-triangle" style="color: var(--MI_THEME-warn);"></i> <SearchText>{{ i18n.ts._serverSettings.userGeneratedContentsVisibilityForVisitor_description2 }}</SearchText></div>
						</template>
					</MkSelect>
				</SearchMarker>

				<XServerRules/>

				<SearchMarker :keywords="['minimum', 'username', 'length']">
					<MkFolder>
						<template #icon><SearchIcon><i class="ti ti-letter-case"></i></SearchIcon></template>
						<template #label><SearchLabel>{{ i18n.ts.minimumUsernameLength }}</SearchLabel></template>

						<div class="_gaps">
							<MkInput v-model="minimumUsernameLength" type="number" :min="MIN_USERNAME_LENGTH" :max="MAX_USERNAME_LENGTH">
								<template #caption>{{ i18n.ts.minimumUsernameLengthDescription }}</template>
							</MkInput>
							<MkButton primary @click="save_minimumUsernameLength">{{ i18n.ts.save }}</MkButton>
						</div>
					</MkFolder>
				</SearchMarker>

				<SearchMarker :keywords="['preserved', 'usernames']">
					<MkFolder>
						<template #icon><SearchIcon><i class="ti ti-lock-star"></i></SearchIcon></template>
						<template #label><SearchLabel>{{ i18n.ts.preservedUsernames }}</SearchLabel></template>

						<div class="_gaps">
							<MkTextarea v-model="preservedUsernames">
								<template #caption>{{ i18n.ts.preservedUsernamesDescription }}</template>
							</MkTextarea>
							<MkButton primary @click="save_preservedUsernames">{{ i18n.ts.save }}</MkButton>
						</div>
					</MkFolder>
				</SearchMarker>

				<SearchMarker :keywords="['sensitive', 'words']">
					<MkFolder>
						<template #icon><SearchIcon><i class="ti ti-message-exclamation"></i></SearchIcon></template>
						<template #label><SearchLabel>{{ i18n.ts.sensitiveWords }}</SearchLabel></template>

						<div class="_gaps">
							<MkTextarea v-model="sensitiveWords">
								<template #caption>{{ i18n.ts.sensitiveWordsDescription }}<br>{{ i18n.ts.sensitiveWordsDescription2 }}</template>
							</MkTextarea>
							<MkButton primary @click="save_sensitiveWords">{{ i18n.ts.save }}</MkButton>
						</div>
					</MkFolder>
				</SearchMarker>

				<SearchMarker :keywords="['prohibited', 'words']">
					<MkFolder>
						<template #icon><SearchIcon><i class="ti ti-message-x"></i></SearchIcon></template>
						<template #label><SearchLabel>{{ i18n.ts.prohibitedWords }}</SearchLabel></template>

						<div class="_gaps">
							<MkTextarea v-model="prohibitedWords">
								<template #caption>{{ i18n.ts.prohibitedWordsDescription }}<br>{{ i18n.ts.prohibitedWordsDescription2 }}</template>
							</MkTextarea>
							<MkButton primary @click="save_prohibitedWords">{{ i18n.ts.save }}</MkButton>
						</div>
					</MkFolder>
				</SearchMarker>

				<SearchMarker :keywords="['prohibited', 'name', 'user']">
					<MkFolder>
						<template #icon><SearchIcon><i class="ti ti-user-x"></i></SearchIcon></template>
						<template #label><SearchLabel>{{ i18n.ts.prohibitedWordsForNameOfUser }}</SearchLabel></template>

						<div class="_gaps">
							<MkTextarea v-model="prohibitedWordsForNameOfUser">
								<template #caption>{{ i18n.ts.prohibitedWordsForNameOfUserDescription }}<br>{{ i18n.ts.prohibitedWordsDescription2 }}</template>
							</MkTextarea>
							<MkButton primary @click="save_prohibitedWordsForNameOfUser">{{ i18n.ts.save }}</MkButton>
						</div>
					</MkFolder>
				</SearchMarker>

				<SearchMarker :keywords="['hidden', 'tags', 'hashtags']">
					<MkFolder>
						<template #icon><SearchIcon><i class="ti ti-eye-off"></i></SearchIcon></template>
						<template #label><SearchLabel>{{ i18n.ts.hiddenTags }}</SearchLabel></template>

						<div class="_gaps">
							<MkTextarea v-model="hiddenTags">
								<template #caption>{{ i18n.ts.hiddenTagsDescription }}</template>
							</MkTextarea>
							<MkButton primary @click="save_hiddenTags">{{ i18n.ts.save }}</MkButton>
						</div>
					</MkFolder>
				</SearchMarker>

				<SearchMarker :keywords="['silenced', 'servers', 'hosts']">
					<MkFolder>
						<template #icon><SearchIcon><i class="ti ti-eye-off"></i></SearchIcon></template>
						<template #label><SearchLabel>{{ i18n.ts.silencedInstances }}</SearchLabel></template>

						<div class="_gaps">
							<MkTextarea v-model="silencedHosts">
								<template #caption>{{ i18n.ts.silencedInstancesDescription }}</template>
							</MkTextarea>
							<MkButton primary @click="save_silencedHosts">{{ i18n.ts.save }}</MkButton>
						</div>
					</MkFolder>
				</SearchMarker>

				<SearchMarker :keywords="['media', 'silenced', 'servers', 'hosts']">
					<MkFolder>
						<template #icon><SearchIcon><i class="ti ti-eye-off"></i></SearchIcon></template>
						<template #label><SearchLabel>{{ i18n.ts.mediaSilencedInstances }}</SearchLabel></template>

						<div class="_gaps">
							<MkTextarea v-model="mediaSilencedHosts">
								<template #caption>{{ i18n.ts.mediaSilencedInstancesDescription }}</template>
							</MkTextarea>
							<MkButton primary @click="save_mediaSilencedHosts">{{ i18n.ts.save }}</MkButton>
						</div>
					</MkFolder>
				</SearchMarker>

				<SearchMarker :keywords="['blocked', 'servers', 'hosts']">
					<MkFolder>
						<template #icon><SearchIcon><i class="ti ti-ban"></i></SearchIcon></template>
						<template #label><SearchLabel>{{ i18n.ts.blockedInstances }}</SearchLabel></template>

						<div class="_gaps">
							<MkTextarea v-model="blockedHosts">
								<template #caption>{{ i18n.ts.blockedInstancesDescription }}</template>
							</MkTextarea>
							<MkButton primary @click="save_blockedHosts">{{ i18n.ts.save }}</MkButton>
						</div>
					</MkFolder>
				</SearchMarker>
			</div>
		</SearchMarker>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import { ref, computed, toRaw } from 'vue';
import * as Misskey from 'misskey-js';
import XServerRules from './server-rules.vue';
import MkSwitch from '@/components/MkSwitch.vue';
import MkA from '@/components/global/MkA.vue';
import MkInput from '@/components/MkInput.vue';
import MkTextarea from '@/components/MkTextarea.vue';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { fetchInstance } from '@/instance.js';
import { i18n } from '@/i18n.js';
import { definePage } from '@/page.js';
import { useMkSelect } from '@/composables/use-mkselect.js';
import MkButton from '@/components/MkButton.vue';
import FormLink from '@/components/form/link.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkSelect from '@/components/MkSelect.vue';
import MkRadios from '@/components/MkRadios.vue';
import type { MkRadiosOption } from '@/components/MkRadios.vue';
import { registrationModeOf, registrationModePatch } from '@/utility/registration-mode.js';
import type { RegistrationMode } from '@/utility/registration-mode.js';
import { MAX_USERNAME_LENGTH, MIN_USERNAME_LENGTH } from '@/utility/local-username.js';

const meta = await misskeyApi('admin/meta');

// mk-go 独自の meta なので misskey-js の型集合には無い (#2557 / #3186)。
const mkgoMeta = meta as unknown as Record<string, unknown>;

const registrationMode = ref<RegistrationMode | null>(registrationModeOf({
	closed: mkgoMeta.registrationClosed === true,
	approval: mkgoMeta.approvalRequiredForSignup === true,
	disableRegistration: meta.disableRegistration,
}));

// 承認制の値そのもの。「受け付けない」の間も残る (再開後に戻る) ので、申請フォームの
// 項目はこちらで出し分ける。
const approvalEnabled = ref(mkgoMeta.approvalRequiredForSignup === true);

const registrationModeDef = [
	{ value: 'open', label: i18n.ts._mkgoRegistration.open, caption: i18n.ts._mkgoRegistration.openCaption },
	{ value: 'invite', label: i18n.ts._mkgoRegistration.invite, caption: i18n.ts._mkgoRegistration.inviteCaption },
	{ value: 'approval', label: i18n.ts._mkgoRegistration.approval, caption: i18n.ts._mkgoRegistration.approvalCaption },
	{ value: 'closed', label: i18n.ts._mkgoRegistration.closed, caption: i18n.ts._mkgoRegistration.closedCaption },
] as const satisfies MkRadiosOption<RegistrationMode>[];

// mk-go: 申請フォームの項目 (#2570)。上限はサーバー側 (ValidateForm) と揃える。
type SignupFormField = { label: string; type: 'text' | 'textarea'; required: boolean };
const maxFormFields = 10;
const signupApplicationForm = ref<SignupFormField[]>(
	Array.isArray((meta as unknown as Record<string, unknown>).signupApplicationForm)
		? structuredClone(toRaw((meta as unknown as Record<string, unknown>).signupApplicationForm)) as SignupFormField[]
		: []);

const fieldTypeDef = [
	{ label: '1 行', value: 'text' },
	{ label: '複数行', value: 'textarea' },
];

function addField() {
	if (signupApplicationForm.value.length >= maxFormFields) return;
	signupApplicationForm.value.push({ label: '', type: 'text', required: false });
}

function removeField(i: number) {
	signupApplicationForm.value.splice(i, 1);
}

function saveSignupApplicationForm() {
	os.apiWithDialog('admin/update-meta', {
		signupApplicationForm: signupApplicationForm.value,
	}).then(() => {
		fetchInstance(true);
	});
}

const emailRequiredForSignup = ref(meta.emailRequiredForSignup);
const {
	model: ugcVisibilityForVisitor,
	def: ugcVisibilityForVisitorDef,
} = useMkSelect({
	items: [
		{ label: i18n.ts._serverSettings._userGeneratedContentsVisibilityForVisitor.all, value: 'all' },
		{ label: i18n.ts._serverSettings._userGeneratedContentsVisibilityForVisitor.localOnly, value: 'local' },
		{ label: i18n.ts._serverSettings._userGeneratedContentsVisibilityForVisitor.none, value: 'none' },
	],
	initialValue: meta.ugcVisibilityForVisitor,
});
const sensitiveWords = ref(meta.sensitiveWords.join('\n'));
const prohibitedWords = ref(meta.prohibitedWords.join('\n'));
const prohibitedWordsForNameOfUser = ref(meta.prohibitedWordsForNameOfUser.join('\n'));
const hiddenTags = ref(meta.hiddenTags.join('\n'));
const preservedUsernames = ref(meta.preservedUsernames.join('\n'));
// mk-go: ユーザー名の最小文字数 (#3015)。純正 backend の admin/meta には無い
// フィールドなので、取れないときは既定値の 1 (= 制限なし) に倒す。
const minimumUsernameLength = ref<number>(
	typeof (meta as unknown as Record<string, unknown>).minimumUsernameLength === 'number'
		? (meta as unknown as Record<string, unknown>).minimumUsernameLength as number
		: MIN_USERNAME_LENGTH);
const blockedHosts = ref(meta.blockedHosts.join('\n'));
const silencedHosts = ref(meta.silencedHosts?.join('\n') ?? '');
const mediaSilencedHosts = ref(meta.mediaSilencedHosts.join('\n'));

async function onChange_registrationMode(value: RegistrationMode) {
	if (value === registrationMode.value) return;
	if (value === 'open') {
		const { canceled } = await os.confirm({
			type: 'warning',
			text: i18n.ts.acknowledgeNotesAndEnable,
		});
		if (canceled) return;
	} else if (value === 'closed') {
		const { canceled } = await os.confirm({
			type: 'warning',
			text: i18n.ts._mkgoRegistration.closeConfirm,
		});
		if (canceled) return;
	}

	// **失敗したら表示を戻す。** 楽観更新のまま放置すると、画面だけ「受け付けない」に
	// なって実際には受け付け続けているのに、管理者は止めたと読む。update-meta は保存に
	// 成功したら必ず 204 なので、reject = 未保存として戻してよい。
	//
	// エラーの提示は apiWithDialog が担う。ただし通信断のように `err.code` が無い
	// 失敗ではその中の分岐が先に落ちてダイアログが出ない (upstream 由来)。その場合も
	// 表示は実状態へ戻る。
	const prev = registrationMode.value;
	registrationMode.value = value;
	try {
		await os.apiWithDialog('admin/update-meta', registrationModePatch[value]);
	} catch {
		registrationMode.value = prev;
		return;
	}
	const approval = registrationModePatch[value].approvalRequiredForSignup;
	if (approval != null) approvalEnabled.value = approval;
	fetchInstance(true);
}

function onChange_emailRequiredForSignup(value: boolean) {
	os.apiWithDialog('admin/update-meta', {
		emailRequiredForSignup: value,
	}).then(() => {
		fetchInstance(true);
	});
}

function onChange_ugcVisibilityForVisitor(value: typeof ugcVisibilityForVisitor.value) {
	os.apiWithDialog('admin/update-meta', {
		ugcVisibilityForVisitor: value,
	}).then(() => {
		fetchInstance(true);
	});
}

function save_minimumUsernameLength() {
	os.apiWithDialog('admin/update-meta', {
		minimumUsernameLength: Number(minimumUsernameLength.value),
	}).then(() => {
		fetchInstance(true);
	});
}

function save_preservedUsernames() {
	os.apiWithDialog('admin/update-meta', {
		preservedUsernames: preservedUsernames.value.split('\n'),
	}).then(() => {
		fetchInstance(true);
	});
}

function save_sensitiveWords() {
	os.apiWithDialog('admin/update-meta', {
		sensitiveWords: sensitiveWords.value.split('\n'),
	}).then(() => {
		fetchInstance(true);
	});
}

function save_prohibitedWords() {
	os.apiWithDialog('admin/update-meta', {
		prohibitedWords: prohibitedWords.value.split('\n'),
	}).then(() => {
		fetchInstance(true);
	});
}

function save_prohibitedWordsForNameOfUser() {
	os.apiWithDialog('admin/update-meta', {
		prohibitedWordsForNameOfUser: prohibitedWordsForNameOfUser.value.split('\n'),
	}).then(() => {
		fetchInstance(true);
	});
}

function save_hiddenTags() {
	os.apiWithDialog('admin/update-meta', {
		hiddenTags: hiddenTags.value.split('\n'),
	}).then(() => {
		fetchInstance(true);
	});
}

function save_blockedHosts() {
	os.apiWithDialog('admin/update-meta', {
		blockedHosts: blockedHosts.value.split('\n') || [],
	}).then(() => {
		fetchInstance(true);
	});
}

function save_silencedHosts() {
	os.apiWithDialog('admin/update-meta', {
		silencedHosts: silencedHosts.value.split('\n') || [],
	}).then(() => {
		fetchInstance(true);
	});
}

function save_mediaSilencedHosts() {
	os.apiWithDialog('admin/update-meta', {
		mediaSilencedHosts: mediaSilencedHosts.value.split('\n') || [],
	}).then(() => {
		fetchInstance(true);
	});
}

const headerTabs = computed(() => []);

definePage(() => ({
	title: i18n.ts.moderation,
	icon: 'ti ti-shield',
}));
</script>

<style lang="scss" module>
.formField {
	padding: 16px;
	border: 1px solid var(--MI_THEME-divider);
	border-radius: var(--MI-radius);
}
</style>
