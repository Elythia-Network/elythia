<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div class="_spacer" style="--MI_SPACER-w: 800px;">
	<div class="_gaps">
		<MkSelect v-model="filter" :items="filterDef">
			<template #label>{{ i18n.ts.state }}</template>
		</MkSelect>

		<MkLoading v-if="fetching"/>
		<MkInfo v-else-if="applications.length === 0">{{ i18n.ts._emojiApplication.noApplications }}</MkInfo>

		<div v-else class="_gaps_s">
			<MkFolder v-for="app in applications" :key="app.id" :defaultOpen="filter === 'pending'">
				<template #icon><i :class="app.remoteHost ? 'ti ti-world-download' : 'ti ti-mood-smile'"></i></template>
				<template #label><span class="_monospace">:{{ app.name }}:</span></template>
				<template #suffix>
					<span :class="[$style.status, $style[app.status]]">{{ statusLabel(app.status) }}</span>
				</template>

				<div class="_gaps_s">
					<!--
						**名前の重複を承認より前に出す。** 押してから DUPLICATE_NAME で
						落ちると、押した側にも申請者にも何も残らない。nameConflict が
						null なのは「DB 障害で確認できなかった」で、重複なし (false) とは
						別物として扱う。
					-->
					<MkInfo v-if="app.nameConflict" warn>
						{{ i18n.tsx._emojiApplication.nameConflict({ name: app.name }) }}
					</MkInfo>
					<MkInfo v-else-if="app.nameConflict === null" warn>
						{{ i18n.ts._emojiApplication.nameConflictUnknown }}
					</MkInfo>

					<div :class="$style.preview">
						<div :class="$style.previewBox">
							<img v-if="previewUrls.get(app.id)" :src="previewUrls.get(app.id)!" :alt="app.name" :class="$style.previewImg" @error="onPreviewError(app)"/>
							<!--
								**null は「確認できなかった」。** 空文字 (申請者が消した) と
								区別する — 障害中に「画像がありません」と出すと、実際には
								存在する申請を却下しうる。
							-->
							<!--
								**「確認できなかった」を先に見る (レビュー R2-H2)。** DB 障害を
								「もう無い」と表示すると、実際には存在する申請を却下しうる。
								remoteGone は null = 確認できず / true = 消えた / false = ある。
							-->
							<!--
								**読み込みに失敗したものを「ありません」に落とさない (レビュー Low 1)。**
								行は存在していて画像だけが出なかった状態なので、「画像がありません」
								と出すと存在する申請を却下しうる (このファイルが remoteGone と
								nameConflict に対して採っているのと同じ判断)。imageUnknown は
								「もう一度読み込んでください」まで言うので、そのまま使える。
							-->
							<span v-else-if="brokenPreviews.has(app.id) || app.url === null || app.remoteGone === null" :class="$style.previewGone">{{ i18n.ts._emojiApplication.imageUnknown }}</span>
							<span v-else-if="app.remoteGone" :class="$style.previewGone">{{ i18n.ts._emojiApplication.remoteGoneShort }}</span>
							<span v-else :class="$style.previewGone">{{ i18n.ts._emojiApplication.imageGone }}</span>
						</div>
						<div :class="$style.previewMeta">
							<!--
								**本文中の実寸を併記する。** 大きな升目だけで審査すると、
								本文で潰れて読めない絵文字を通してしまう。カスタム絵文字は
								本文では文字サイズの 1.25 倍でしか描かれない。
							-->
							<div :class="$style.previewLabel">{{ i18n.ts._emojiApplication.inlinePreview }}</div>
							<div v-if="previewUrls.get(app.id)" :class="$style.inlineSample">
								{{ i18n.ts._emojiApplication.inlineSampleText }}
								<img :src="previewUrls.get(app.id)!" :alt="app.name" :class="$style.inlineImg" @error="onPreviewError(app)"/>
							</div>
							<div v-if="app.fileType" :class="$style.previewType">{{ app.fileType }}</div>
						</div>
					</div>

					<!--
						**リモートは取り込み元を出す (#2935)。** 承認すると
						`admin/emoji/copy` と同じ経路でインポートされるので、
						どのサーバーの何を取るのかが見えないと判断できない。
					-->
					<MkKeyValue v-if="app.remoteHost" oneline>
						<template #key>{{ i18n.ts._emojiApplication.remoteSource }}</template>
						<template #value><span class="_monospace">:{{ app.remoteName }}:@{{ app.remoteHost }}</span></template>
					</MkKeyValue>
					<MkKeyValue oneline>
						<template #key>{{ i18n.ts._emojiApplication.applicant }}</template>
						<template #value><MkA :to="`/admin/user/${app.userId}`">{{ app.userId }}</MkA></template>
					</MkKeyValue>
					<MkKeyValue oneline>
						<template #key>{{ i18n.ts.createdAt }}</template>
						<template #value><MkTime :time="app.createdAt" mode="detail"/></template>
					</MkKeyValue>
					<MkKeyValue v-if="app.category" oneline>
						<template #key>{{ i18n.ts.category }}</template>
						<template #value>{{ app.category }}</template>
					</MkKeyValue>
					<MkKeyValue v-if="app.aliases.length > 0" oneline>
						<template #key>{{ i18n.ts.tags }}</template>
						<template #value>{{ app.aliases.join(' ') }}</template>
					</MkKeyValue>
					<MkKeyValue oneline>
						<template #key>{{ i18n.ts.license }}</template>
						<template #value>{{ app.license }}</template>
					</MkKeyValue>
					<MkKeyValue oneline>
						<template #key>{{ i18n.ts.sensitive }}</template>
						<template #value>{{ app.isSensitive ? i18n.ts.yes : i18n.ts.no }}</template>
					</MkKeyValue>
					<MkKeyValue v-if="app.comment" oneline>
						<template #key>{{ i18n.ts._emojiApplication.comment }}</template>
						<template #value>{{ app.comment }}</template>
					</MkKeyValue>
					<MkKeyValue v-if="app.rejectReason" oneline>
						<template #key>{{ i18n.ts._emojiApplication.rejectReason }}</template>
						<template #value>{{ app.rejectReason }}</template>
					</MkKeyValue>

					<!--
						**判断材料はボタンの前に置く (#2960)。** 押した後に出しても
						意味が無い。同じ名前・同じ取り込み元・同じ画像で過去に
						却下されていたら、その理由をここで見せる。
					-->
					<XRelated :applicationId="app.id"/>

					<template v-if="app.status === 'pending'">
						<MkTextarea v-model="reasons[app.id]">
							<template #label>{{ i18n.ts._emojiApplication.rejectReason }}</template>
							<template #caption>{{ i18n.ts._emojiApplication.rejectReasonCaption }}</template>
						</MkTextarea>
						<div :class="$style.actions">
							<MkButton primary :disabled="busy === app.id" @click="approve(app)">
								{{ i18n.ts._emojiApplication.approve }}
							</MkButton>
							<MkButton danger :disabled="busy === app.id" @click="reject(app)">
								{{ i18n.ts._emojiApplication.reject }}
							</MkButton>
						</div>
					</template>
				</div>
			</MkFolder>
		</div>
	</div>
</div>
</template>

<script lang="ts" setup>
import { computed, ref, watch } from 'vue';
import MkButton from '@/components/MkButton.vue';
import MkFolder from '@/components/MkFolder.vue';
import XRelated from '@/pages/admin/custom-emojis-manager.application-related.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkSelect from '@/components/MkSelect.vue';
import MkTextarea from '@/components/MkTextarea.vue';
import MkLoading from '@/components/global/MkLoading.vue';
import MkTime from '@/components/global/MkTime.vue';
import MkA from '@/components/global/MkA.vue';
import { i18n } from '@/i18n.js';
import { getProxiedImageUrl } from '@/utility/media-proxy.js';
import { useMkSelect } from '@/composables/use-mkselect.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import * as os from '@/os.js';

type Application = {
	id: string;
	userId: string;
	status: 'pending' | 'approved' | 'rejected' | 'canceled';
	name: string;
	category: string | null;
	aliases: string[];
	license: string;
	isSensitive: boolean;
	comment: string | null;
	rejectReason?: string;
	createdAt: string;
	// null = 確認できなかった (DB 障害)。空文字 = 申請者が drive から消した。
	url: string | null;
	fileType: string | null;
	// kind = remote (#2935) のときだけ入る。
	remoteHost?: string;
	remoteName?: string;
	// true = 消えている (承認すると落ちる) / false = ある / null = 確認できなかった。
	remoteGone?: boolean | null;
	// true = 同名あり / false = なし / null = 確認できなかった (DB 障害)。
	// **null を false と同一視しない** — 確認できていないことを隠すと、
	// 承認を押してから落ちる。
	nameConflict?: { emojiId: string; createdAt: string | null } | false | null;
};

// MkSelect は items API を使う (option 子要素は受け付けない)。
const { model: filter, def: filterDef } = useMkSelect({
	items: [
		{ label: i18n.ts._emojiApplication.filterPending, value: 'pending' },
		{ label: i18n.ts._emojiApplication.filterProcessed, value: 'processed' },
		{ label: i18n.ts.all, value: 'all' },
	],
	initialValue: 'pending',
});
const applications = ref<Application[]>([]);
const reasons = ref<Record<string, string>>({});
const fetching = ref(true);
const busy = ref<string | null>(null);

// **読み込みに失敗した申請の id (レビュー Low 1)。** `<img>` は失敗しても
// 壊れた画像のまま黙っているので、審査画面では「画像がある」ように見えてしまう。
// 実際には proxy が 403 を返す (allowlist から外れた) 場合などがある。
const brokenPreviews = ref<Set<string>>(new Set());

function onPreviewError(app: Application): void {
	// Set を差し替えないと template が再評価されない。
	brokenPreviews.value = new Set(brokenPreviews.value).add(app.id);
}

/**
 * Resolves the image to show for an application.
 *
 * **リモートの生 URL は本番の CSP で読めない (レビュー R2-H3)。**
 * `img-src 'self' data: blob:` を enforce しているので、任意の origin の画像は
 * ブロックされる。media proxy を通すと同一オリジンになり、allowlist が
 * `emoji.originalUrl` / `publicUrl` を通すので解決できる (`MkCustomEmoji` が
 * 既に同じ形)。drive のファイル (kind = own) は自サーバーなのでそのまま。
 */
function resolvePreviewUrl(app: Application): string | null {
	if (app.url == null || app.url === '') return null;
	if (brokenPreviews.value.has(app.id)) return null;
	if (!app.remoteHost) return app.url;
	return getProxiedImageUrl(app.url, 'emoji', false, true);
}

// **1 行につき 1 回だけ解決する (レビュー Low 2)。** template から関数を呼ぶと
// 再描画のたびに行数 x 4 回 `getProxiedImageUrl` が走り、URL を組み直した結果
// `<img>` の src が同値でも別インスタンスになる。
const previewUrls = computed(() => {
	const map = new Map<string, string | null>();
	for (const app of applications.value) {
		map.set(app.id, resolvePreviewUrl(app));
	}
	return map;
});

function statusLabel(status: Application['status']): string {
	switch (status) {
		case 'pending': return i18n.ts._emojiApplication.statusPending;
		case 'approved': return i18n.ts._emojiApplication.statusApproved;
		case 'rejected': return i18n.ts._emojiApplication.statusRejected;
		default: return i18n.ts._emojiApplication.statusCanceled;
	}
}

async function load() {
	fetching.value = true;
	// 読み込み直したら失敗の記録も捨てる。残すと、直ったあとも
	// 「確認できませんでした」が出続ける。
	brokenPreviews.value = new Set();
	try {
		applications.value = await misskeyApi('admin/emoji-application/list' as never, {
			filter: filter.value,
			limit: 50,
		} as never) as unknown as Application[];
	} catch (err) {
		// 一覧が引けなかったことを黙らせない。空の一覧と区別が付かなくなる。
		applications.value = [];
		await os.alert({ type: 'error', text: reviewErrorText(err) });
	} finally {
		fetching.value = false;
	}
}

// reviewErrorText maps the API error code onto a message.
//
// 種別を潰さない。「失敗しました」だけだと、名前の重複なのか画像が消えたのか、
// 他のモデレーターが先に処理したのかが分からない。
function reviewErrorText(err: unknown): string {
	switch ((err as { code?: string } | null)?.code) {
		case 'DUPLICATE_NAME': return i18n.ts._emojiApplication.errorDuplicateName;
		case 'ALREADY_PROCESSED': return i18n.ts._emojiApplication.errorAlreadyProcessed;
		case 'UNSUPPORTED_FILE_TYPE': return i18n.ts._emojiApplication.errorUnsupportedType;
		case 'NO_SUCH_FILE': return i18n.ts._emojiApplication.errorFileGone;
		// **承認だけが失敗する形 (#2966)。** 申請の時点でも同じ上限で弾くので
		// 普通は届かないが、ロールの上限を後から下げると既存の申請がここに来る。
		// 汎用文だと「何度押しても失敗する理由」がモデレーターに伝わらない。
		case 'EMOJI_IMAGE_TOO_LARGE': return i18n.ts._emojiApplication.errorImageTooLarge;
		// **この設計がいちばん想定している失敗 (レビュー M3)。** リモート絵文字の
		// 行はキャッシュに近く、審査を待つ間に消えうる。汎用メッセージに潰すと、
		// モデレーターには「なぜか失敗した」としか見えない。
		case 'NO_SUCH_EMOJI': return i18n.ts._emojiApplication.remoteGoneReview;
		default: return i18n.ts.somethingHappened;
	}
}

watch(filter, load, { immediate: true });

async function approve(app: Application) {
	// **重複があるなら押す前に止める。** backend も承認の直前に見ているが、
	// そこで落とすとモデレーターには「なぜか失敗した」としか見えない。
	// **消えている絵文字は押す前に止める。** 押すと NO_SUCH_EMOJI で落ちる。
	// **null (確認できなかった) では止めない (レビュー R2-H2)。** 止めると
	// DB の一時障害中に承認が操作ごとブロックされる。押した結果が
	// NO_SUCH_EMOJI なら、そのとき正しい文言が出る。
	if (app.remoteGone === true) {
		await os.alert({ type: 'warning', text: i18n.ts._emojiApplication.remoteGoneReview });
		return;
	}
	if (app.nameConflict) {
		const { canceled } = await os.confirm({
			type: 'warning',
			text: i18n.tsx._emojiApplication.nameConflictConfirm({ name: app.name }),
		});
		if (canceled) return;
	}
	busy.value = app.id;
	try {
		await misskeyApi('admin/emoji-application/approve' as never, { applicationId: app.id } as never);
		await load();
	} catch (err) {
		// **握り潰さない。** backend は DUPLICATE_NAME / UNSUPPORTED_FILE_TYPE /
		// NO_SUCH_FILE / ALREADY_PROCESSED を種別ごとに返しているのに、catch が
		// 無いと misskeyApi の reject がどこにも出ず、ボタンが戻るだけになる
		// (本番では unhandledrejection のハンドラが _DEV_ の中にあるため無反応)。
		await os.alert({ type: 'error', text: reviewErrorText(err) });
		await load();
	} finally {
		busy.value = null;
	}
}

async function reject(app: Application) {
	const reason = (reasons.value[app.id] ?? '').trim();
	// **理由なしの却下を止める。** 申請者にはこの文面しか届かないので、空だと
	// 「駄目でした」だけが残り、直して出し直す手がかりが無い。
	if (reason === '') {
		await os.alert({
			type: 'warning',
			text: i18n.ts._emojiApplication.rejectReasonRequired,
		});
		return;
	}
	busy.value = app.id;
	try {
		await misskeyApi('admin/emoji-application/reject' as never, {
			applicationId: app.id,
			reason,
		} as never);
		delete reasons.value[app.id];
		await load();
	} catch (err) {
		await os.alert({ type: 'error', text: reviewErrorText(err) });
		await load();
	} finally {
		busy.value = null;
	}
}
</script>

<style lang="scss" module>
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
.previewGone {
	font-size: 0.8em;
	text-align: center;
	opacity: 0.7;
}
.previewMeta {
	display: flex;
	flex-direction: column;
	gap: 6px;
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
.previewType {
	font-size: 0.8em;
	opacity: 0.75;
}

.actions {
	display: flex;
	gap: 8px;
	flex-wrap: wrap;
}
</style>
