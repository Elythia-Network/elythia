<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<MkWindow
	ref="windowEl"
	:initialWidth="400"
	:initialHeight="500"
	:canResize="true"
	@close="windowEl?.close()"
	@closed="emit('closed')"
>
	<template #header>:{{ name }}:</template>

	<div style="display: flex; flex-direction: column; min-height: 100%;">
		<div class="_spacer" style="--MI_SPACER-min: 20px; --MI_SPACER-max: 28px; flex-grow: 1;">
			<div class="_gaps_m">
				<div v-if="imgUrl != null" :class="$style.imgs">
					<div style="background: #000;" :class="$style.imgContainer">
						<img :src="imgUrl" :class="$style.img" :alt="name"/>
					</div>
					<div style="background: #222;" :class="$style.imgContainer">
						<img :src="imgUrl" :class="$style.img" :alt="name"/>
					</div>
					<div style="background: #ddd;" :class="$style.imgContainer">
						<img :src="imgUrl" :class="$style.img" :alt="name"/>
					</div>
					<div style="background: #fff;" :class="$style.imgContainer">
						<img :src="imgUrl" :class="$style.img" :alt="name"/>
					</div>
				</div>

				<MkInput v-if="needsRename" v-model="nameInput">
					<template #label>{{ i18n.ts.name }}</template>
					<template #caption>{{ i18n.ts._remoteEmojiImport.nameCaption }}</template>
				</MkInput>
				<MkKeyValue v-else>
					<template #key>{{ i18n.ts.name }}</template>
					<template #value>{{ name }}</template>
				</MkKeyValue>
				<MkInfo v-if="needsRename" warn>{{ i18n.ts._remoteEmojiImport.invalidName }}</MkInfo>
				<MkKeyValue>
					<template #key>{{ i18n.ts.host }}</template>
					<template #value>{{ host }}</template>
				</MkKeyValue>

				<!--
					mk-go: 取得した値を編集できるようにした (#2698)。AP の Emoji tag は
					`name` / `icon` / `_misskey_license.freeText` しか運ばないので、
					カテゴリ・エイリアス・センシティブは相手の REST API から取る。
					本番の実測では、リモート絵文字 19,129 件のうちそれらは 1 件も
					連合で入っていなかった。
				-->
				<MkInfo v-if="fetchState === 'unsupported'" warn>{{ i18n.ts._remoteEmojiImport.unsupported }}</MkInfo>
				<MkInfo v-else-if="fetchState === 'failed'" warn>{{ i18n.ts._remoteEmojiImport.fetchFailed }}</MkInfo>

				<!--
					**編集フォームはインポート導線のときだけ出す。** 管理画面の「詳細」は
					従来「見て copy するだけ」の経路で、そこにフォームを出すと初期値が
					空のまま Import されて既存のカテゴリ・エイリアスが消える
					(この props は id / name / host / license / url しか持たない)。
				-->
				<template v-if="editable">
				<MkInput v-model="category">
					<template #label>{{ i18n.ts.category }}</template>
				</MkInput>
				<MkInput v-model="aliases">
					<template #label>{{ i18n.ts.tags }}</template>
					<template #caption>{{ i18n.ts._remoteEmojiImport.aliasesCaption }}</template>
				</MkInput>
				<MkTextarea v-model="license">
					<template #label>{{ i18n.ts.license }}</template>
				</MkTextarea>
				<MkSwitch v-model="isSensitive">{{ i18n.ts.markAsSensitive }}</MkSwitch>
				</template>

				<MkKeyValue v-else>
					<template #key>{{ i18n.ts.license }}</template>
					<template #value>{{ license }}</template>
				</MkKeyValue>
			</div>
		</div>
		<div :class="$style.footer">
			<MkButton primary rounded :disabled="!nameOk" style="margin: 0 auto;" @click="done">
				<i class="ti ti-plus"></i> {{ i18n.ts.import }}
			</MkButton>
		</div>
	</div>
</MkWindow>
</template>

<script lang="ts" setup>
import { computed, ref, useTemplateRef } from 'vue';
import type * as Elythia from 'elythia-js';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkButton from '@/components/MkButton.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkInput from '@/components/MkInput.vue';
import MkSwitch from '@/components/MkSwitch.vue';
import MkTextarea from '@/components/MkTextarea.vue';
import MkWindow from '@/components/MkWindow.vue';
import { emptyStrToEmptyArray } from '@/pages/admin/custom-emojis-manager.impl.js';
import { i18n } from '@/i18n.js';
import * as os from '@/os.js';
import { isUsableEmojiName } from '@/utility/emoji-name.js';
import { getProxiedImageUrl, getStaticImageUrl } from '@/utility/media-proxy.js';
import { prefer } from '@/preferences.js';
import type { RemoteEmojiMeta } from '@/utility/import-remote-emoji.js';

const props = defineProps<{
	emoji: {
		id: string,
		name: string,
		host: string,
		license: string | null,
		// **null を取りうる。** backend は emojiRepo 未配線のとき originalUrl
		// キー自体を返さない (emoji_remote_meta.go)。
		url: string | null
	},
	/**
	 * mk-go: 取得済みのリモートメタデータ (#2698)。
	 *
	 * **渡されなければ取りに行かない。** 管理画面の「詳細」からもこのモーダルを
	 * 開くので、ここで自動取得すると**見るだけのつもりの操作が相手サーバーへの
	 * リクエストになる** (一覧で何行も開けばその数だけ出ていく)。取得はインポート
	 * 導線 (`importRemoteEmoji`) 側で 1 回だけ行い、結果をここへ渡す。
	 */
	meta?: RemoteEmojiMeta | null,
}>();

const emit = defineEmits<{
	/**
	 * Emitted after the import succeeded, with the name the emoji was saved
	 * under.
	 *
	 * mk-go (#3187): 取り込み直後にその絵文字でリアクションするために要る。
	 * **名前は直せる** (#2998) ので、呼び出し側が元の名前を使うと別の絵文字を指す。
	 */
	(ev: 'done', name: string): void,
	(ev: 'closed'): void
}>();

const windowEl = useTemplateRef('windowEl');

const name = computed(() => props.emoji.name);
const host = computed(() => props.emoji.host);
/**
 * mk-go: 名前がそのままでは使えないときだけ編集させる (#2998)。
 *
 * backend は `^[a-zA-Z0-9_]+$` と 128 文字を要求するが、**リモートの名前は相手が
 * 決める値**なので満たすとは限らない。満たさないまま取り込むと MFM の `:name:` から
 * 参照できない絵文字ができるので backend が 400 で弾く。**弾かれるだけだと取り込む
 * 手段が無くなる**ので、ここで直せるようにする。
 *
 * **常に編集可能にはしない。** 普通の絵文字で名前を変える動機は無く、欄を出すと
 * 「変えてもよいもの」に見える。
 */
const needsRename = computed(() => !isUsableEmojiName(props.emoji.name));
const nameInput = ref(props.emoji.name);
const nameOk = computed(() => isUsableEmojiName(nameInput.value));
/**
 * Image URL for the preview, routed through the media proxy (#2903).
 *
 * **raw の originalUrl をそのまま `<img src>` に入れると表示されない。**
 * originalUrl は相手サーバー上の URL で、mk-go の CSP は
 * `img-src 'self' data: blob:` なのでブロックされる。通常の絵文字表示
 * (MkCustomEmoji) は media proxy を通しており、モーダルだけが raw を使っていた。
 *
 * 静止画設定 (disableShowingAnimatedImages) の扱いも MkCustomEmoji に揃える。
 */
const imgUrl = computed(() => {
	if (props.emoji.url == null) return null;
	// **noFallback は渡さない。** MkCustomEmoji は @error で `:name:` に落とすが、
	// このモーダルには受け皿が無く、allowlist が 403 を返すと 4 タイルすべてが
	// 壊れ画像アイコンになって理由も出ない。プロキシ側の fallback に任せる。
	const proxied = getProxiedImageUrl(props.emoji.url, 'emoji');
	return prefer.s.disableShowingAnimatedImages ? getStaticImageUrl(proxied) : proxied;
});

// 編集できる項目。**AP 経由で入っているのは license だけ**なので、他は空から始めて
// 取得結果で埋める。
const category = ref('');
const aliases = ref('');
const license = ref(props.emoji.license ?? '');
const isSensitive = ref(false);

// 取得の状態。`unsupported` は相手に per-name endpoint が無い場合 (Mastodon 系)。
// **失敗を異常として扱わない** — 手で埋めれば取り込めるので、理由を出して続行させる。
// `none` は取得を伴わない経路 (管理画面の「詳細」) で、案内も出さない。
// 取得結果を渡された経路 (= インポート導線) でだけ編集させる。
const editable = computed(() => props.meta != null);

const fetchState = computed<'none' | 'done' | 'unsupported' | 'failed'>(() => {
	if (props.meta == null) return 'none';
	if (props.meta.fetched) return 'done';
	return props.meta.reason === 'unsupported' ? 'unsupported' : 'failed';
});

// **返ってきたキーだけを反映する。** 取れなかった項目はキーごと無いので、
// ここで既存値 (license など) を消さない。
if (props.meta?.fetched) {
	if (props.meta.category != null) category.value = props.meta.category;
	if (props.meta.aliases != null) aliases.value = props.meta.aliases.join(' ');
	if (props.meta.license != null) license.value = props.meta.license;
	if (props.meta.isSensitive != null) isSensitive.value = props.meta.isSensitive;
}

async function done() {
	// **上書き項目は mk-go が足した additive パラメータ** (#2698) なので、
	// misskey-js の autogen 型 (`{ emojiId: string }`) には無い。upstream の
	// paramDef は emojiId のみ必須で、足しても既存の呼び出しは通る。型は elythia-js の
	// ElythiaRequestExtensions で重ねてある (#3439)。
	// **編集していない経路では上書きを送らない。** 送ると src の値が空で
	// 潰れる (この props は category / aliases / isSensitive を持たないため、
	// フォームの初期値が空になる)。
	const params: Elythia.Endpoints['admin/emoji/copy']['req'] = { emojiId: props.emoji.id };
	// **名前は直したときだけ送る (#2998)。** そのまま使える名前に上書きを付けると、
	// 送る値が増えるだけで挙動は変わらない。
	if (needsRename.value) {
		params.name = nameInput.value;
	}
	if (editable.value) {
		params.category = category.value;
		params.aliases = emptyStrToEmptyArray(aliases.value);
		params.license = license.value;
		params.isSensitive = isSensitive.value;
	}
	// 失敗すると apiWithDialog がエラーを出して reject するので、下の emit には届かない
	// (= 呼び出し側はリアクションしない)。
	await os.apiWithDialog('admin/emoji/copy', params);

	emit('done', needsRename.value ? nameInput.value : props.emoji.name);
	windowEl.value?.close();
}
</script>

<style lang="scss" module>
.imgs {
	display: flex;
	gap: 8px;
	flex-wrap: wrap;
	justify-content: center;
}

.imgContainer {
	padding: 8px;
	border-radius: 6px;
}

.img {
	display: block;
	height: 64px;
	width: 64px;
	object-fit: contain;
}

.footer {
	position: sticky;
	z-index: 10000;
	bottom: 0;
	left: 0;
	padding: 12px;
	border-top: solid 0.5px var(--MI_THEME-divider);
	background: color(from var(--MI_THEME-bg) srgb r g b / 0.5);
	-webkit-backdrop-filter: var(--MI-blur, blur(15px));
	backdrop-filter: var(--MI-blur, blur(15px));
}
</style>
