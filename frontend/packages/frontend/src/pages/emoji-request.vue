<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<PageWithHeader v-model:tab="tab" :tabs="headerTabs">
	<!--
		**本文カラムでもファイルのドロップを握り潰す (#2959)。** `.preview` は
		最小 124px の帯でしかなく、そこを外すとブラウザが画像を開いて**入力中の
		内容ごと失う**。ドロップを案内する以上、外す機会はこの変更で増えている。
		受け取るのはプレビューの上だけなので、ここでは握り潰すだけ。

		**カラムの外 (左右の余白・ヘッダ・タブバー) までは守れない。** `._spacer`
		は `max-width: 700px` の中央カラムで、アプリ全体のドロップガードは
		存在しない (`@drop` を持つのは `ui/deck/column.vue` だけ)。他のページも
		同じ状態なので、ここだけ全面にするのはこの変更の範囲を超える。
	-->
	<div class="_spacer" style="--MI_SPACER-w: 700px;" @dragover="onPageDragover" @drop="onPageDrop">
		<!-- ===== 申請する ===== -->
		<div v-if="tab === 'apply'" class="_gaps">
			<MkInfo>{{ i18n.ts._emojiApplication.applyNote }}</MkInfo>

			<!--
				**ドロップ対象はプレビュー全体 (#2959)。** 升目だけだと的が小さく、
				外すと本文カラムの握り潰しに落ちる (カラムの外まで外すとブラウザが
				画像を開いて**入力中の内容ごと失う**)。**`preventDefault` は
				`dragover` にも要る** — 呼ばないと `drop` が発火しない。modifier では
				なくハンドラ内で条件付きに呼ぶ (ファイル以外まで止めると deck の
				カラム並べ替えを潰すため)。
			-->
			<div
				:class="[$style.preview, { [$style.dragover]: draghover }]"
				@dragover="onDragover"
				@dragenter="onDragenter"
				@dragleave="onDragleave"
				@drop="onDrop"
			>
				<div :class="$style.previewBox">
					<img v-if="file" :src="file.url" :alt="name" :class="$style.previewImg"/>
					<i v-else class="ti ti-photo" :class="$style.previewPlaceholder"></i>
				</div>
				<div :class="$style.previewMeta">
					<!--
						**アップロード中でも押させる (レビュー H1)。** ここを塞ぐと、
						アップロードが返ってこないときに操作手段が 1 つも無くなる。
						`chooseFile` が世代を進めて `uploading` も解除するので、
						遅れて届いたドロップの結果に上書きされることも、
						`canSubmit` が false のままになることも無い。
					-->
					<MkButton @click="chooseFile">{{ i18n.ts._emojiApplication.chooseImage }}</MkButton>
					<div v-if="uploading" :class="$style.previewLabel">
						<MkLoading :em="true"/> {{ i18n.ts._emojiApplication.dropUploading }}
					</div>
					<!--
						**導線を書いておく。** ドロップできることは見ただけでは
						分からない。**ただしタッチ端末では出さない** — 実行できない
						操作を案内することになる。
					-->
					<div v-else-if="canDrop" :class="$style.previewLabel">{{ i18n.ts._emojiApplication.dropImage }}</div>
					<!--
						**本文中の実寸を見せる。** カスタム絵文字は本文では文字サイズの
						1.25 倍でしか描かれない。大きな升目だけを見て申請すると、細い線や
						小さな文字が潰れたものを出してしまう。
					-->
					<template v-if="file">
						<div :class="$style.previewLabel">{{ i18n.ts._emojiApplication.inlinePreview }}</div>
						<div :class="$style.inlineSample">
							{{ i18n.ts._emojiApplication.inlineSampleText }}
							<img :src="file.url" :alt="name" :class="$style.inlineImg"/>
						</div>
					</template>
					<div :class="$style.previewLabel">{{ i18n.ts._emojiApplication.imageHint }}</div>
				</div>
			</div>

			<MkInput v-model="name" :spellcheck="false">
				<template #label>{{ i18n.ts.name }}</template>
				<template #caption>{{ i18n.ts._emojiApplication.nameCaption }}</template>
			</MkInput>
			<MkInfo v-if="name !== '' && !nameValid" warn>{{ i18n.ts._emojiApplication.nameInvalid }}</MkInfo>

			<MkInput v-model="category">
				<template #label>{{ i18n.ts.category }}</template>
			</MkInput>
			<MkInput v-model="aliases">
				<template #label>{{ i18n.ts.tags }}</template>
				<template #caption>{{ i18n.ts._emojiApplication.aliasesCaption }}</template>
			</MkInput>
			<MkTextarea v-model="license">
				<template #label>{{ i18n.ts.license }}</template>
				<template #caption>{{ i18n.ts._emojiApplication.licenseCaption }}</template>
			</MkTextarea>
			<MkSwitch v-model="isSensitive">
				{{ i18n.ts.sensitive }}
			</MkSwitch>
			<MkTextarea v-model="comment">
				<template #label>{{ i18n.ts._emojiApplication.comment }}</template>
				<template #caption>{{ i18n.ts._emojiApplication.commentCaption }}</template>
			</MkTextarea>

			<MkButton primary :disabled="!canSubmit" @click="submit">
				{{ i18n.ts._emojiApplication.submit }}
			</MkButton>
		</div>

		<!-- ===== 自分の申請 ===== -->
		<div v-else class="_gaps">
			<MkLoading v-if="fetching"/>
			<MkInfo v-else-if="mine.length === 0">{{ i18n.ts._emojiApplication.noneOfMine }}</MkInfo>
			<div v-else class="_gaps_s">
				<MkFolder v-for="app in mine" :key="app.id" :defaultOpen="app.status === 'rejected'">
					<!--
						**折りたたんだ状態でも画像で見分けられるようにする (#2989)。**
						名前だけだと、似た名前の申請・再申請・リモート申請を見比べた
						ときにどれがどれか分からない。画像を出せないときは従来の
						種別アイコンへ戻す。
					-->
					<template #icon>
						<img v-if="previewUrls.get(app.id)" :src="previewUrls.get(app.id)!" :alt="app.name" :class="$style.mineThumb" loading="lazy" @error="onPreviewError(app)"/>
						<i v-else :class="app.remoteHost ? 'ti ti-world-download' : 'ti ti-mood-smile'"></i>
					</template>
					<template #label><span class="_monospace">:{{ app.name }}:</span></template>
					<template #suffix>
						<span :class="[$style.status, $style[app.status]]">{{ statusLabel(app.status) }}</span>
					</template>

					<div class="_gaps_s">
						<!--
							**「無い」の理由を出し分ける (#2989)。** 申請元が消えた /
							承認後の絵文字が消された / 確認できなかった / 読み込みに
							失敗した、は利用者に伝えるべきことが違う。API が
							`preview.state` で返すので、こちらで推測しない。
						-->
						<div :class="$style.minePreviewBox">
							<img v-if="previewUrls.get(app.id)" :src="previewUrls.get(app.id)!" :alt="app.name" :class="$style.minePreviewImg" loading="lazy" @error="onPreviewError(app)"/>
							<span v-else :class="$style.minePreviewGone">{{ previewMessage(app) }}</span>
						</div>
						<MkKeyValue v-if="app.remoteHost" oneline>
							<template #key>{{ i18n.ts._emojiApplication.remoteSource }}</template>
							<template #value><span class="_monospace">:{{ app.remoteName }}:@{{ app.remoteHost }}</span></template>
						</MkKeyValue>
						<MkKeyValue oneline>
							<template #key>{{ i18n.ts.createdAt }}</template>
							<template #value><MkTime :time="app.createdAt" mode="detail"/></template>
						</MkKeyValue>
						<MkKeyValue v-if="app.processedAt" oneline>
							<template #key>{{ i18n.ts._emojiApplication.processedAt }}</template>
							<template #value><MkTime :time="app.processedAt" mode="detail"/></template>
						</MkKeyValue>
						<!--
							却下理由はモデレーターが書いた文面をそのまま出す。直して
							出し直すための唯一の手がかりなので、要約しない。
						-->
						<MkInfo v-if="app.rejectReason" warn>{{ app.rejectReason }}</MkInfo>
						<MkInfo v-else-if="app.status === 'approved'">
							{{ i18n.tsx._emojiApplication.approvedNote({ name: app.name }) }}
						</MkInfo>

						<MkButton v-if="app.status === 'pending'" danger @click="cancel(app)">
							{{ i18n.ts._emojiApplication.cancel }}
						</MkButton>
					</div>
				</MkFolder>
			</div>
		</div>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import { computed, ref, watch } from 'vue';
import MkButton from '@/components/MkButton.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkInput from '@/components/MkInput.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkSwitch from '@/components/MkSwitch.vue';
import MkTextarea from '@/components/MkTextarea.vue';
import MkLoading from '@/components/global/MkLoading.vue';
import MkTime from '@/components/global/MkTime.vue';
import { i18n } from '@/i18n.js';
import { applicationPreviewMessage, applicationPreviewUrl } from '@/utility/emoji-request-preview.js';
import type { ApplicationPreview } from '@/utility/emoji-request-preview.js';
import { definePage } from '@/page.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { selectFile, uploadFile } from '@/utility/drive.js';
import { prefer } from '@/preferences.js';
import { deviceKind } from '@/utility/device-kind.js';
import { pickDroppedEmojiImage, droppedEmojiImageErrorText } from '@/utility/emoji-image-drop.js';
import { emojiApplicationQuotaText, emojiApplicationPendingLimitText } from '@/utility/emoji-application-quota.js';
import * as os from '@/os.js';

type Application = {
	id: string;
	status: 'pending' | 'approved' | 'rejected' | 'canceled';
	name: string;
	rejectReason?: string;
	createdAt: string;
	processedAt: string | null;
	// kind = remote (#2935) のときだけ入る。
	remoteHost?: string;
	remoteName?: string;
	// 画像の解決結果 (#2989)。**URL が空かどうかで状態を推測しない** —
	// 「削除された」「確認できなかった」「承認後の絵文字が消された」は
	// 出す文面が違う。
	preview?: ApplicationPreview;
};

// upstream の admin/emoji/add と同じ制約。**申請側で先に弾く** — 承認まで
// 通してから登録で落ちると、モデレーターが押した後にエラーになる。
const NAME_RE = /^[a-zA-Z0-9_]+$/;

const tab = ref<'apply' | 'mine'>('apply');
const headerTabs = computed(() => [{
	key: 'apply',
	title: i18n.ts._emojiApplication.tabApply,
}, {
	key: 'mine',
	title: i18n.ts._emojiApplication.tabMine,
}]);

const file = ref<{ id: string; url: string } | null>(null);
const name = ref('');
const category = ref('');
const aliases = ref('');
const license = ref('');
const isSensitive = ref(false);
const comment = ref('');

const mine = ref<Application[]>([]);
const fetching = ref(false);

// **読み込みに失敗した申請の id (#2989)。** `<img>` は失敗しても壊れた画像の
// まま黙っているので、出ていないのに「ある」ように見える。API 上は存在するので
// 「削除済み」とは別の文面を出す (審査画面 #2957 と同じ判断)。
const brokenPreviews = ref<Set<string>>(new Set());

function onPreviewError(app: Application): void {
	// Set を差し替えないと template が再評価されない。
	brokenPreviews.value = new Set(brokenPreviews.value).add(app.id);
}

// **1 行につき 1 回だけ解決する。** template から関数を呼ぶと再描画のたびに
// `getProxiedImageUrl` が走り、同値でも別の URL インスタンスになる。
const previewUrls = computed(() => {
	const map = new Map<string, string | null>();
	for (const app of mine.value) {
		map.set(app.id, applicationPreviewUrl(app.preview, brokenPreviews.value.has(app.id)));
	}
	return map;
});

function previewMessage(app: Application): string {
	return applicationPreviewMessage(app.preview, brokenPreviews.value.has(app.id));
}

const nameValid = computed(() => NAME_RE.test(name.value));
// ライセンスも必須。出典が辿れないと、問題が出たときに消すしか手が無くなる。
// アップロード中は押させない — 完了前に送ると古い fileId で申請される。
const canSubmit = computed(() => !uploading.value && file.value != null && nameValid.value && license.value.trim() !== '');

// ドロップの案内はデスクトップだけに出す (タッチでは実行できない)。
const canDrop = deviceKind === 'desktop';
const draghover = ref(false);
const uploading = ref(false);
// **ドロップの世代。** アップロード中に選び直したり別の画像を落としたとき、
// 遅れて届いた前の結果で上書きさせない。
let dropGeneration = 0;

async function chooseFile(ev: MouseEvent) {
	const selected = await selectFile({ anchorElement: ev.currentTarget ?? ev.target, multiple: false });
	// **世代を進める。** 進めないと、進行中のドロップが後から届いたときに
	// **いま選んだものを黙って上書きする**。申請されるのは利用者が最後に
	// 選んだ画像ではなくなり、画面上はそれが正しく見える。
	dropGeneration++;
	// **アップロード中の表示も解除する。** 返ってこないアップロードに
	// 引きずられたままだと `canSubmit` が false のままで、選び直しても
	// 一生申請できない (それがこのボタンを塞がない理由そのもの)。
	uploading.value = false;
	file.value = { id: selected.id, url: selected.url };
}

function isFileDrag(ev: DragEvent): boolean {
	return ev.dataTransfer != null && Array.from(ev.dataTransfer.items).some(i => i.kind === 'file');
}

function onDragover(ev: DragEvent) {
	if (ev.dataTransfer == null) return;
	// **ファイル以外には一切触らない。** `preventDefault` も `dropEffect` も
	// しないので、deck のカラム並べ替えはそのまま祖先へ届く。
	if (!isFileDrag(ev)) {
		draghover.value = false;
		return;
	}
	ev.preventDefault();
	// **伝播を止める。** 祖先の `onPageDragover` が後から `dropEffect` を
	// `none` へ上書きしてしまう (dragover はバブルするので親が後に走る)。
	ev.stopPropagation();
	// **`effectAllowed` と突き合わせる。** 許されていない operation を指定すると
	// drag operation が none になり、**光ったまま落とせない**状態になる。
	switch (ev.dataTransfer.effectAllowed) {
		case 'all':
		case 'uninitialized':
		case 'copy':
		case 'copyLink':
		case 'copyMove':
			ev.dataTransfer.dropEffect = 'copy';
			break;
		case 'move':
		case 'linkMove':
			ev.dataTransfer.dropEffect = 'move';
			break;
		default:
			ev.dataTransfer.dropEffect = 'link';
			break;
	}
	draghover.value = true;
}

/**
 * Swallows file drops in the body column so the browser does not navigate.
 *
 * **ファイルのときだけ `preventDefault` する。** 無条件に止めると、deck の
 * カラム並べ替えのような**このページが関知しないドロップまで潰す**
 * (`ui/deck/column.vue` が `@drop.prevent.stop` で受けている)。
 *
 * **守れるのは本文カラムだけ** — 付いている `._spacer` は `max-width: 700px`
 * の中央カラムで、左右の余白・ヘッダ・タブバーは素通りする。
 */
function onPageDragover(ev: DragEvent) {
	if (!isFileDrag(ev)) return;
	ev.preventDefault();
	// 受け取らないので「ここには落とせない」と示す。既定の copy が残ると、
	// 落とせるように見えて何も起きない。
	if (ev.dataTransfer != null) ev.dataTransfer.dropEffect = 'none';
}

function onPageDrop(ev: DragEvent) {
	if (isFileDrag(ev)) ev.preventDefault();
}

function onDragenter(ev: DragEvent) {
	// **ここでも種別を見る。** 無条件に光らせると `onDragover` のガードが
	// 素通りになり、テキスト選択のドラッグでも必ず先にハイライトが点く。
	if (isFileDrag(ev)) draghover.value = true;
}

function onDragleave(ev: DragEvent) {
	// **子要素へ入っただけの dragleave では消さない。** `.stop` は伝播を
	// 止めるだけで、下から上がってきた dragleave のハンドラ実行は止まらない。
	const next = ev.relatedTarget;
	if (next instanceof Node && ev.currentTarget instanceof Node && ev.currentTarget.contains(next)) return;
	draghover.value = false;
}

/**
 * Uploads a dropped image to drive and selects it (#2959).
 *
 * **drive を経由する。** 申請 API は drive の fileId を受ける契約なので、
 * ここだけ multipart にすると承認側 (`admin/emoji/add` と同じ経路) の検証を
 * 迂回することになる。
 */
async function onDrop(ev: DragEvent) {
	draghover.value = false;
	// ファイル以外は祖先 (deck のカラムなど) に任せる。
	if (!isFileDrag(ev)) return;
	ev.preventDefault();
	ev.stopPropagation();

	const picked = pickDroppedEmojiImage(Array.from(ev.dataTransfer?.files ?? []));
	if (!picked.ok) {
		// **理由を出す。** 「失敗しました」だけだと、形式が悪いのか件数が多いのか
		// 分からず、同じ操作を繰り返すことになる。
		if (picked.reason === 'none') return;
		await os.alert({ type: 'error', text: droppedEmojiImageErrorText(picked.reason) });
		return;
	}

	const generation = ++dropGeneration;
	uploading.value = true;
	try {
		// **フォルダの設定を尊重する。** 「画像を選ぶ」は uploader 経由で
		// `prefer.s.uploadFolder` に入るので、こちらだけ直下に落とすと
		// 同じ操作で置き場所が変わる。
		const uploaded = await uploadFile(picked.file, { folderId: prefer.s.uploadFolder }).filePromise;
		// **世代が進んでいたら捨てる。** 追い越された古いアップロードの結果で
		// 新しい選択を上書きしない。
		if (generation !== dropGeneration) return;
		// **置き換える。** 既に選んでいたものを残すと、どちらが申請されるのか
		// 分からない。
		file.value = { id: uploaded.id, url: uploaded.url };
	} catch {
		// **ここではダイアログを出さない (レビュー M1)。** `uploadFile` は
		// 中断以外の失敗で自分でダイアログを出す (サイズ超過・非 200・通信
		// エラー) ので、重ねると 2 枚出る。中断はそもそも黙って戻る形。
	} finally {
		if (generation === dropGeneration) uploading.value = false;
	}
}

async function submit() {
	try {
		await misskeyApi('emoji-application/create' as never, {
			name: name.value,
			category: category.value,
			aliases: aliases.value.split(/\s+/).filter(a => a !== ''),
			license: license.value,
			isSensitive: isSensitive.value,
			fileId: file.value?.id,
			comment: comment.value,
		} as never);
	} catch (err) {
		// **エラーの種別をそのまま出す。** 「失敗しました」だけだと、名前を
		// 直せばよいのか別の問題なのかが分からない。
		await os.alert({ type: 'error', text: submitErrorText(err) });
		return;
	}
	await os.alert({ type: 'success', text: i18n.ts._emojiApplication.submitted });
	file.value = null;
	name.value = '';
	category.value = '';
	aliases.value = '';
	license.value = '';
	isSensitive.value = false;
	comment.value = '';
	tab.value = 'mine';
}

function submitErrorText(err: unknown): string {
	const code = (err as { code?: string } | null)?.code;
	switch (code) {
		case 'DUPLICATE_NAME': return i18n.ts._emojiApplication.errorDuplicateName;
		case 'ALREADY_REQUESTED': return i18n.ts._emojiApplication.errorAlreadyRequested;
		case 'INVALID_EMOJI_NAME': return i18n.ts._emojiApplication.nameInvalid;
		case 'LICENSE_REQUIRED': return i18n.ts._emojiApplication.licenseCaption;
		case 'NO_SUCH_FILE': return i18n.ts._emojiApplication.errorNoFile;
		// H2 / M8 で create から新しく到達可能になった分。既定に落とすと
		// 「何か問題が」だけになり、直しようが分からない。
		case 'TOO_LONG': return i18n.ts._emojiApplication.errorTooLong;
		case 'UNSUPPORTED_FILE_TYPE': return i18n.ts._emojiApplication.errorUnsupportedType;
		// policy を持たない人が URL 直打ちで開いた場合。導線は設定側で隠して
		// いるが、ページ自体には gate が無い。
		case 'ROLE_PERMISSION_DENIED': return i18n.ts._emojiApplication.errorNotAllowed;
		// **レート制限は新しく到達可能になった (レビュー R2-M3)。** 汎用の
		// 「何かがおかしいようです」に潰すと、待てば通ることが分からない。
		case 'RATE_LIMIT_EXCEEDED': return i18n.ts._emojiApplication.errorRateLimited;
		// ロールごとの期間上限 (#2958)。API の 1 時間あたりの制限とは別で、
		// こちらは日・週・月の単位。
		case 'EMOJI_APPLICATION_QUOTA_EXCEEDED': return emojiApplicationQuotaText(err);
		// 審査待ちの上限 (#2977)。期間の上限とは解決の仕方が違う (待つのではなく
		// 取り下げるか、結果が出るのを待つ)。
		case 'EMOJI_APPLICATION_PENDING_LIMIT_EXCEEDED': return emojiApplicationPendingLimitText(err);
		default: return i18n.ts.somethingHappened;
	}
}

function statusLabel(status: Application['status']): string {
	switch (status) {
		case 'pending': return i18n.ts._emojiApplication.statusPending;
		case 'approved': return i18n.ts._emojiApplication.statusApproved;
		case 'rejected': return i18n.ts._emojiApplication.statusRejected;
		default: return i18n.ts._emojiApplication.statusCanceled;
	}
}

async function loadMine() {
	// **読み込み直したら失敗の記録も捨てる (#2989)。** 残すと、proxy の一時的な
	// 失敗で 1 行が欠けたあと、画像が復旧してもその行だけ「読み込めません」の
	// まま戻らない (審査画面が同じ判断をしている)。
	brokenPreviews.value = new Set();
	fetching.value = true;
	try {
		mine.value = await misskeyApi('emoji-application/list-mine' as never, { limit: 50 } as never) as unknown as Application[];
	} catch {
		mine.value = [];
		await os.alert({ type: 'error', text: i18n.ts.somethingHappened });
	} finally {
		fetching.value = false;
	}
}

async function cancel(app: Application) {
	const { canceled } = await os.confirm({
		type: 'warning',
		text: i18n.ts._emojiApplication.cancelConfirm,
	});
	if (canceled) return;
	try {
		await misskeyApi('emoji-application/cancel' as never, { applicationId: app.id } as never);
	} catch (err) {
		// 既に審査されていた場合など。黙って一覧を引き直すと「押しても何も
		// 起きない」ように見える。
		await os.alert({ type: 'error', text: cancelErrorText(err) });
	}
	await loadMine();
}

function cancelErrorText(err: unknown): string {
	switch ((err as { code?: string } | null)?.code) {
		case 'ALREADY_PROCESSED': return i18n.ts._emojiApplication.errorAlreadyProcessed;
		case 'NO_SUCH_APPLICATION': return i18n.ts._emojiApplication.errorNoSuchApplication;
		default: return i18n.ts.somethingHappened;
	}
}

watch(tab, (v) => {
	if (v === 'mine') void loadMine();
});

definePage(computed(() => ({
	title: i18n.ts._emojiApplication.title,
	icon: 'ti ti-mood-plus',
})));
</script>

<style lang="scss" module>
/*
	「自分の申請」のプレビュー (#2989)。

	**クラス名を申請タブと分ける。** この CSS module には申請タブ用の
	`.preview` / `.previewImg` が既にあり、同じ名前で足すと後から来る側が勝つ。
	実際それで枠が `grid-template-columns: 96px 1fr` になり、画像が左カラムへ
	寄って中央から外れていた (本番で指摘された)。

	枠の作りは審査画面 (`custom-emojis-manager.applications.vue`) の
	`.previewBox` と同じ — 透過画像は `--MI_THEME-bg` が panel と対比するので、
	白い絵文字でも輪郭が分かる。
*/
.mineThumb {
	width: 1.4em;
	height: 1.4em;
	object-fit: contain;
	vertical-align: middle;
}

.minePreviewBox {
	width: 96px;
	height: 96px;
	display: grid;
	place-items: center;
	border-radius: var(--MI-radius-sm);
	background: var(--MI_THEME-bg);
}

.minePreviewImg {
	max-width: 96px;
	max-height: 96px;
	object-fit: contain;
}

.minePreviewGone {
	padding: 0 8px;
	font-size: 0.8em;
	text-align: center;
	opacity: 0.7;
}

.status {
	font-size: 0.9em;
	font-weight: bold;
}
.pending { color: var(--MI_THEME-warn); }
.approved { color: var(--MI_THEME-success); }
.rejected { color: var(--MI_THEME-error); }
.canceled { opacity: 0.7; }

.preview {
	display: grid;
	grid-template-columns: 96px 1fr;
	gap: 16px;
	align-items: start;
	padding: 14px;
	border: solid 1px var(--MI_THEME-divider);
	border-radius: var(--MI-radius-sm);
}
/* **ドロップ可能であることを枠で示す。** 透明度や影だけだと、背景の明るい
   テーマで見えない。 */
.dragover {
	outline: 2px dashed var(--MI_THEME-accent);
	outline-offset: 4px;
}

.previewBox {
	width: 96px;
	height: 96px;
	display: grid;
	place-items: center;
	border-radius: var(--MI-radius-sm);
	background: var(--MI_THEME-bg);
}
.previewImg {
	max-width: 96px;
	max-height: 96px;
	object-fit: contain;
}
.previewPlaceholder {
	font-size: 32px;
	opacity: 0.4;
}
.previewMeta {
	display: flex;
	flex-direction: column;
	gap: 8px;
	min-width: 0;
}
.previewLabel {
	font-size: 0.85em;
	opacity: 0.75;
}
.inlineSample {
	line-height: 1.6;
}
.inlineImg {
	height: 1.25em;
	vertical-align: -0.25em;
}
</style>
