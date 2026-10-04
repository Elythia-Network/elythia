<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<SearchMarker path="/settings/avatar-decoration" :label="i18n.ts.avatarDecorations" :keywords="['avatar', 'icon', 'decoration', 'emoji', '絵文字']" icon="ti ti-sparkles">
	<div>
		<div v-if="!loading" class="_gaps">
			<MkInfo>{{ i18n.tsx._profile.avatarDecorationMax({ max: $i.policies.avatarDecorationLimit }) }} ({{ i18n.tsx.remainingN({ n: $i.policies.avatarDecorationLimit - $i.avatarDecorations.length }) }})</MkInfo>

			<MkAvatar :class="$style.avatar" :user="$i" forceShowDecoration/>

			<div v-if="$i.avatarDecorations.length > 0" v-panel :class="$style.current" class="_gaps_s">
				<div>{{ i18n.ts.inUse }}</div>

				<div :class="$style.decorations">
					<XDecoration
						v-for="(avatarDecoration, i) in $i.avatarDecorations"
						:decoration="decorationForEntry(avatarDecoration)"
						:angle="avatarDecoration.angle"
						:flipH="avatarDecoration.flipH"
						:offsetX="avatarDecoration.offsetX"
						:offsetY="avatarDecoration.offsetY"
						:scale="entryScale(avatarDecoration)"
						:active="true"
						@click="openAttachedDecoration(i)"
					/>
				</div>

				<MkButton danger @click="detachAllDecorations">{{ i18n.ts.detachAll }}</MkButton>
			</div>

			<!--
				カスタム絵文字をデコレーションとして使う (#2975、mk-go 独自)。
				**個数は上の avatarDecorationLimit に合算される**ので、ここに
				専用の残数表示は置かない (2 つの残数が並ぶと、どちらが効くのか
				読めなくなる)。
			-->
			<div v-if="canUseEmojiAsDecoration" v-panel :class="$style.current" class="_gaps_s">
				<div>{{ i18n.ts.customEmojis }}</div>
				<MkButton rounded @click="pickEmojiDecoration"><i class="ti ti-mood-smile"></i> {{ i18n.ts._mkgoAvatarDecoration.chooseEmoji }}</MkButton>
				<div :class="$style.emojiCaption">{{ i18n.ts._mkgoAvatarDecoration.emojiCaption }}</div>
			</div>

			<!--
				**policy を外されたのに装着済みが残っている状態を説明する。**
				検証は配列の全要素に掛かるので、絵文字を着けたまま別の装飾を
				編集しようとすると RESTRICTED_BY_ROLE で落ちる (catalog 由来の
				roleIdsThatCanBeUsedThisDecoration と同じ挙動)。導線を隠すだけ
				だと、利用者には原因が何も見えない。
			-->
			<MkInfo v-else-if="wornEmojiCount > 0" warn>{{ i18n.tsx._mkgoAvatarDecoration.emojiDisabledButWorn({ n: wornEmojiCount }) }}</MkInfo>
			<MkFoldableSection v-for="category in Object.keys(groupedDecorations)" :key="category" :expanded="true">
				<template #header>{{ category || i18n.ts.other }}</template>
				<div :class="$style.decorations">
					<XDecoration
						v-for="avatarDecoration in groupedDecorations[category]"
						:key="avatarDecoration.id"
						:decoration="avatarDecoration"
						@click="openDecoration(avatarDecoration)"
					/>
				</div>
			</MkFoldableSection>
		</div>
		<div v-else>
			<MkLoading/>
		</div>
	</div>
</SearchMarker>
</template>

<script lang="ts" setup>
import { ref, defineAsyncComponent, computed } from 'vue';
import * as Misskey from 'misskey-js';
import XDecoration from './avatar-decoration.decoration.vue';
import XDialog from './avatar-decoration.dialog.vue';
import MkButton from '@/components/MkButton.vue';
import MkFoldableSection from '@/components/MkFoldableSection.vue';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { i18n } from '@/i18n.js';
import { ensureSignin } from '@/i.js';
import MkInfo from '@/components/MkInfo.vue';
import { definePage } from '@/page.js';
import { groupAvatarDecorations } from '@/utility/group-avatar-decorations.js';
import { customEmojisMap } from '@/custom-emojis.js';

const $i = ensureSignin();

/**
 * 画面が扱うデコレーションの形。catalog 由来 (`get-avatar-decorations` の 1 件) と
 * 絵文字由来 (#2975) の両方をこれで表す。絵文字由来はロール制限を持たないので
 * `roleIdsThatCanBeUsedThisDecoration` は常に空。
 */
type DecorationLike = {
	id: string;
	url: string;
	name: string;
	roleIdsThatCanBeUsedThisDecoration: string[];
};

const unknownDecoration: DecorationLike = { id: '', url: '', name: '?', roleIdsThatCanBeUsedThisDecoration: [] };

/**
 * mk-go 独自 policy (#2975)。misskey-js の autogen 型には無いのでキャストで読む
 * (独自 endpoint を `as never` で呼ぶのと同じ扱い)。**既定は true** で、backend の
 * effectivepolicy の既定と揃えてある — 揃えないと policy を明示していない
 * サーバーで導線だけ消える。
 */
const canUseEmojiAsDecoration = computed(() => {
	const value = ($i.policies as Record<string, unknown>).canUseEmojiAsAvatarDecoration;
	return typeof value === 'boolean' ? value : true;
});

/**
 * avatarDecorations の要素に載る mk-go 独自 field (#2975)。絵文字由来のときだけ
 * 入る。これも autogen 型に無いのでキャストで読む。
 */
function entryEmojiName(entry: unknown): string | null {
	const name = (entry as { emojiName?: unknown }).emojiName;
	return typeof name === 'string' && name !== '' ? name : null;
}

/**
 * `avatarDecorations` の要素に載る mk-go 独自の大きさ (#2975)。省略で 1。
 * autogen 型に無いのでキャストで読む。
 */
function entryScale(entry: unknown): number | undefined {
	const scale = (entry as { scale?: unknown }).scale;
	return typeof scale === 'number' ? scale : undefined;
}

/** 装着中のうち絵文字由来の件数。policy を失ったときの注意書きに使う。 */
const wornEmojiCount = computed(() => $i.avatarDecorations.filter(d => entryEmojiName(d) != null).length);

/**
 * 装着中の 1 件を表示用のデコレーションへ変換する。
 *
 * **絵文字由来は catalog を引かない。** 引くと必ず外れて `?` になる
 * (`avatar_decoration` には絵文字の行が無い)。
 */
function decorationForEntry(entry: { id: string; url: string }): DecorationLike {
	const emojiName = entryEmojiName(entry);
	if (emojiName != null) {
		return { id: entry.id, url: entry.url, name: `:${emojiName}:`, roleIdsThatCanBeUsedThisDecoration: [] };
	}
	return avatarDecorations.value.find(d => d.id === entry.id) ?? unknownDecoration;
}

const loading = ref(true);
const avatarDecorations = ref<Misskey.entities.GetAvatarDecorationsResponse>([]);
const groupedDecorations = computed(() => groupAvatarDecorations(avatarDecorations.value));

misskeyApi('get-avatar-decorations').then(_avatarDecorations => {
	avatarDecorations.value = _avatarDecorations;
	loading.value = false;
});

function openAttachedDecoration(index: number) {
	const entry = $i.avatarDecorations[index];
	openDecoration(decorationForEntry(entry), index, entryEmojiName(entry));
}

/**
 * カスタム絵文字を新しく装着するときの既定の大きさ (#2975)。
 *
 * `MkAvatar` の `.decoration` はアバターの **2 倍**の枠に描かれる
 * (`top:-50%; left:-50%; width:200%`)。管理者が登録するデコレーションは
 * その枠に合わせて余白込みで作られているが、カスタム絵文字は余白の無い
 * 正方形なので、既定のままだとアイコンを完全に覆ってしまう。0.5 でちょうど
 * アバターと同じ大きさになる。
 */
const EMOJI_DEFAULT_SCALE = 0.5;

async function openDecoration(avatarDecoration: DecorationLike, index?: number, emojiName?: string | null) {
	// **絵文字由来は `emojiName` だけを送る** (#2975)。backend は指定があると
	// `id` を読まないので、装着済みの行から来た古い id を持ち回っても混ざらない。
	// `id` / `url` を残すのは、保存の応答を待たずに手元の $i を描き替えるため。
	const buildEntry = (payload: { angle: number; flipH: boolean; offsetX: number; offsetY: number; scale: number }) => ({
		id: avatarDecoration.id,
		url: avatarDecoration.url,
		angle: payload.angle,
		flipH: payload.flipH,
		offsetX: payload.offsetX,
		offsetY: payload.offsetY,
		scale: payload.scale,
		...(emojiName != null ? { emojiName } : {}),
	});

	const { dispose } = os.popup(XDialog, {
		decoration: avatarDecoration,
		usingIndex: index ?? null,
		// 新規の絵文字だけ小さめから始める。装着済み (index あり) は保存値を
		// 使うので、この既定は効かない。
		defaultScale: emojiName != null ? EMOJI_DEFAULT_SCALE : undefined,
	}, {
		'attach': async (payload) => {
			const update = [...$i.avatarDecorations, buildEntry(payload)];
			await os.apiWithDialog('i/update', {
				avatarDecorations: update,
			}, undefined, emojiDecorationErrors());
			$i.avatarDecorations = update;
		},
		'update': async (payload) => {
			const update = [...$i.avatarDecorations];
			update[index!] = buildEntry(payload);
			await os.apiWithDialog('i/update', {
				avatarDecorations: update,
			}, undefined, emojiDecorationErrors());
			$i.avatarDecorations = update;
		},
		'detach': async () => {
			// **残りの配列をまるごと送り直す**ので、外した以外の絵文字が
			// センシティブ化 / policy 変更で弾かれうる。detachAll は `[]` を
			// 送るので検証のループが 1 度も回らず、こちらだけが該当する。
			const update = [...$i.avatarDecorations];
			update.splice(index!, 1);
			await os.apiWithDialog('i/update', {
				avatarDecorations: update,
			}, undefined, emojiDecorationErrors());
			$i.avatarDecorations = update;
		},
		closed: () => dispose(),
	});
}

/**
 * 絵文字ピッカーから 1 件選んで、通常のデコレーションと同じ調整ダイアログへ渡す
 * (#2975)。
 *
 * **ピッカーは Unicode 絵文字も返す。** カスタム絵文字だけが対象なので、
 * `customEmojisMap` に無いものはここで弾く (backend も弾くが、選んだ直後に
 * 理由が出るほうが分かりやすい)。`customEmojis` は `/api/emojis` 由来 =
 * ローカル絵文字だけなので、リモート絵文字はそもそも候補に出ない。
 */
async function pickEmojiDecoration(ev: PointerEvent) {
	const anchor = (ev.currentTarget ?? ev.target) as HTMLElement;
	const picked = await os.pickEmoji(anchor, { showPinned: false });
	if (!picked) return;

	// ピッカーはカスタム絵文字を `:name:` で返す。Unicode 絵文字は文字そのもの。
	const name = picked.startsWith(':') && picked.endsWith(':') ? picked.slice(1, -1) : null;
	if (name == null) {
		await os.alert({ type: 'error', text: i18n.ts._mkgoAvatarDecoration.customEmojiOnly });
		return;
	}
	// **「カスタム絵文字ではない」と「もう無いカスタム絵文字」を分ける。**
	// 「最近使った絵文字」には削除済みの名前が残るので、そこから選ぶと
	// map に無い。前者の文面を出すと、利用者は正しい操作をしたのに
	// 「カスタム絵文字を選べ」と言われることになる。
	const emoji = customEmojisMap.get(name);
	if (emoji == null) {
		await os.alert({ type: 'error', text: i18n.tsx._mkgoAvatarDecoration.noSuchEmoji({ name }) });
		return;
	}
	// **センシティブな絵文字は使えない。** backend も設定時と表示時の両方で
	// 弾くので、ここは理由を先に見せるためだけのもの。
	if (emoji.isSensitive) {
		await os.alert({ type: 'error', text: i18n.ts._mkgoAvatarDecoration.sensitiveEmojiNotAllowed });
		return;
	}
	// **ロール制限はここでは見ない (mk-go #3240)。** `$i.roles` には公開ロールしか
	// 入らない (本家と同じ) ので、非公開ロールに限った絵文字を、そのロールを持つ
	// 本人にまで弾いてしまう。判定は backend (全ロールで見る) に任せ、弾かれたら
	// 「装着」のときに emojiRestrictedByRole を出す (下の customErrors)。

	openDecoration({
		id: '',
		url: emoji.url,
		name: `:${emoji.name}:`,
		roleIdsThatCanBeUsedThisDecoration: [],
	}, undefined, emoji.name);
}

/**
 * `i/update` が絵文字デコレーションで返すエラーの文面 (#2975)。
 *
 * **キーは code ではなく `err.id` (UUID)。** `os.apiWithDialog` は
 * `customErrors[err.id]` を引く (`os.ts`)。code を書くと一度も一致せず、
 * 既定の「英語のサーバーメッセージ + UUID」がそのまま出る。
 *
 * 手元のチェックは `customEmojis` (= `/api/emojis` のキャッシュ) を見るが、
 * あれは最大 1 時間古いので、直近にセンシティブ化された絵文字は前段を
 * すり抜けてここへ来る。ロール関連は前段で完全には防げない
 * (policy はタブを開いたまま変わりうる)。
 */
const ERR_SENSITIVE_EMOJI_NOT_ALLOWED = '6e5caaca-f206-4822-a565-e3beb0b60a25';
const ERR_NO_SUCH_EMOJI = '684dec9d-a8c2-4364-9aa8-456c49cb1dc8';
const ERR_RESTRICTED_BY_ROLE = '8feff0ba-5ab5-585b-31f4-4df816663fad';

function emojiDecorationErrors(): os.ApiWithDialogCustomErrors {
	return {
		[ERR_SENSITIVE_EMOJI_NOT_ALLOWED]: { text: i18n.ts._mkgoAvatarDecoration.sensitiveEmojiNotAllowed },
		[ERR_NO_SUCH_EMOJI]: { text: i18n.ts._mkgoAvatarDecoration.emojiGone },
		[ERR_RESTRICTED_BY_ROLE]: { text: i18n.ts._mkgoAvatarDecoration.emojiRestrictedByRole },
	};
}

function detachAllDecorations() {
	os.confirm({
		type: 'warning',
		text: i18n.ts.areYouSure,
	}).then(async ({ canceled }) => {
		if (canceled) return;
		await os.apiWithDialog('i/update', {
			avatarDecorations: [],
		});
		$i.avatarDecorations = [];
	});
}

const headerActions = computed(() => []);

const headerTabs = computed(() => []);

definePage(() => ({
	title: i18n.ts.avatarDecorations,
	icon: 'ti ti-sparkles',
}));
</script>

<style lang="scss" module>
.avatar {
	display: inline-block;
	width: 72px;
	height: 72px;
	margin: 16px auto;
}

.current {
	padding: 16px;
	border-radius: var(--MI-radius);
}

.emojiCaption {
	font-size: 0.85em;
	opacity: 0.7;
}

.decorations {
	display: grid;
	grid-template-columns: repeat(auto-fill, minmax(140px, 1fr));
	grid-gap: 12px;
}
</style>
