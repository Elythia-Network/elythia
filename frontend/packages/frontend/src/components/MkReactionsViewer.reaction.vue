<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<button
	ref="buttonEl"
	v-ripple="canReact"
	class="_button"
	:class="[$style.root, { [$style.reacted]: myReaction == reaction, [$style.canToggle]: canReact, [$style.small]: prefer.s.reactionsDisplaySize === 'small', [$style.large]: prefer.s.reactionsDisplaySize === 'large' }]"
	@click="toggleReaction()"
	@contextmenu.prevent.stop="menu"
>
	<MkReactionIcon style="pointer-events: none;" :class="prefer.s.limitWidthOfReaction ? $style.limitWidth : ''" :reaction="reaction" :emojiUrl="reactionEmojis[emojiName]"/>
	<span :class="$style.count">{{ count }}</span>
</button>
</template>

<script lang="ts" setup>
import { computed, inject, onBeforeUnmount, onMounted, useTemplateRef, watch } from 'vue';
import * as Misskey from 'misskey-js';
import { getUnicodeEmojiOrNull } from '@@/js/emojilist.js';
import { getEmojiNameFromReaction, isLocalCustomEmojiReaction } from '@@/js/emoji-name.js';
import MkCustomEmojiDetailedDialog from './MkCustomEmojiDetailedDialog.vue';
import type { MenuItem } from '@/types/menu';
import XDetails from '@/components/MkReactionsViewer.details.vue';
import MkReactionIcon from '@/components/MkReactionIcon.vue';
import { importRemoteEmoji, importRemoteEmojiAndReact, canImportRemoteEmoji, hasLocalEmojiWithSameName, bareEmojiName } from '@/utility/import-remote-emoji.js';
import { requestRemoteEmojiImport } from '@/utility/request-remote-emoji.js';
import { bindLongPress } from '@/utility/long-press.js';
import { localAlternativeReaction } from '@/utility/reaction-alternative.js';
import * as os from '@/os.js';
import { misskeyApi, misskeyApiGet } from '@/utility/misskey-api.js';
import { useTooltip } from '@/composables/use-tooltip.js';
import { $i } from '@/i.js';
import MkReactionEffect from '@/components/MkReactionEffect.vue';
import { i18n } from '@/i18n.js';
import * as sound from '@/utility/sound.js';
// import { checkReactionPermissions } from '@/utility/check-reaction-permissions.js';
import { customEmojis, customEmojisMap } from '@/custom-emojis.js';
import { prefer } from '@/preferences.js';
import { DI } from '@/di.js';
import { noteEvents } from '@/composables/use-note-capture.js';
import { mute as muteEmoji, unmute as unmuteEmoji, checkMuted as isEmojiMuted } from '@/utility/emoji-mute.js';
import { addToEmojiPalette } from '@/utility/emoji-palette.js';
import { haptic } from '@/utility/haptic.js';

const props = defineProps<{
	noteId: Misskey.entities.Note['id'];
	reaction: string;
	reactionEmojis: Misskey.entities.Note['reactionEmojis'];
	myReaction: Misskey.entities.Note['myReaction'];
	count: number;
	isInitial: boolean;
}>();

const mock = inject(DI.mock, false);

const emit = defineEmits<{
	(ev: 'reactionToggled', emoji: string, newCount: number): void;
}>();

const buttonEl = useTemplateRef('buttonEl');

const emojiName = computed(() => getEmojiNameFromReaction(props.reaction));

const isLocalCustomEmoji = computed(() => isLocalCustomEmojiReaction(props.reaction));

const canToggle = computed(() => {
	const emoji = isLocalCustomEmoji.value ? customEmojisMap.get(emojiName.value) : getUnicodeEmojiOrNull(props.reaction);

	// TODO
	//return $i != null && emoji != null && checkReactionPermissions($i, props.note, emoji);
	return $i != null && emoji != null;
});

// mk-go: リモートのリアクションにローカルの同名絵文字で相乗りする (#2697)。
//
// **backend は変更しない。** リモート利用者が送ってくる `:foo@their.host:` は
// そのホストの絵文字を指しているので、backend を「ローカル優先」に変えると AP の
// 受信側が別の絵文字として記録する。相乗りは**送る側の話**で、送るショートコードを
// frontend が選び直せば足りる。
//
// **数は合算されない。** リアクションは文字列キーで持つので、`:foo@host:` を押すと
// `:foo@.:` のチップが別に増える (既にローカルの同名チップがあればそちらが増える)。
// **`customEmojis` を読んで reactive 依存を作る。** 判定の中で使う
// `customEmojisMap` は素の `Map` なので、これが無いと #2698 の導線でその場で
// 絵文字をインポートしてもリロードするまでチップが押せるようにならない。
const localAlternative = computed(() => {
	if (!prefer.s.reactableRemoteReactionEnabled) return null;
	void customEmojis.value;
	return localAlternativeReaction(props.reaction);
});

// **`$i` を見る。** 見ないと未ログインでも押せる見た目になるが、`toggleReaction` は
// `$i == null` で抜けるので押しても何も起きない (設定はアカウントに紐付かない
// localStorage なので、on にして sign out したブラウザで踏む)。
const canReact = computed(() => canToggle.value || ($i != null && localAlternative.value != null));

// 実際に送るリアクション。相乗りのときだけ `props.reaction` と違う。
const sendingReaction = computed(() => localAlternative.value ?? props.reaction);

async function toggleReaction() {
	if (!canReact.value) return;
	await sendReaction(sendingReaction.value, localAlternative.value == null);
}

/**
 * Sends (or withdraws) the given reaction on the note, asking first when it
 * replaces or cancels the current one.
 *
 * mk-go (#3187): チップを押したときと「インポートしてリアクション」の両方から使う。
 * 後者は取り込んだ絵文字 (`:name@.:`) を送るので、チップ自身のキーとは違う。
 *
 * `ownChip` は「送るのがこのチップ自身のキーか」。mock (チュートリアル) の emit は
 * それが真のときだけ行う。**本体で `props.reaction` と比べない** — 送る文字列の
 * 選び方をチップの identity に戻す変異を、ゲート (reaction_longpress_gate_test.go)
 * が本体の単語で検出しているため。
 */
async function sendReaction(reaction: string, ownChip: boolean) {
	if ($i == null) return;
	const reactionEmojiName = getEmojiNameFromReaction(reaction);

	const me = $i;

	const oldReaction = props.myReaction;
	if (oldReaction) {
		const confirm = await os.confirm({
			type: 'warning',
			text: oldReaction !== reaction ? i18n.ts.changeReactionConfirm : i18n.ts.cancelReactionConfirm,
		});
		if (confirm.canceled) return;

		if (oldReaction !== reaction) {
			sound.playMisskeySfx('reaction');
			haptic();
		}

		if (mock) {
			// **押したチップと違うキーを送るとき (相乗り / インポートしてリアクション)
			// は emit しない。** 親は emit されたキーでチップを引き当てて delta を
			// 計算するので (`MkReactionsViewer.vue` の `onMockToggleReaction`)、
			// 別のチップの count を動かすか no-op になる。mock を渡すのは
			// `MkTutorialDialog.*` = **実利用者が通るチュートリアル**で、example note の
			// reactions は空から始まるので相乗りできるチップは出ないが、契約を壊さない。
			if (ownChip) emit('reactionToggled', reaction, (props.count - 1));
			return;
		}

		misskeyApi('notes/reactions/delete', {
			noteId: props.noteId,
		}).then(() => {
			noteEvents.emit(`unreacted:${props.noteId}`, {
				userId: me.id,
				reaction: oldReaction,
			});
			if (oldReaction !== reaction) {
				misskeyApi('notes/reactions/create', {
					noteId: props.noteId,
					reaction: reaction,
				}).then(() => {
					const emoji = customEmojisMap.get(reactionEmojiName);
					if (emoji == null && getUnicodeEmojiOrNull(reaction) == null) {
						return;
					}
					noteEvents.emit(`reacted:${props.noteId}`, {
						userId: me.id,
						reaction: reaction,
						emoji: emoji,
					});
				});
			}
		});
	} else {
		if (prefer.s.confirmOnReact) {
			const confirm = await os.confirm({
				type: 'question',
				text: i18n.tsx.reactAreYouSure({ emoji: reaction.replace('@.', '') }),
			});

			if (confirm.canceled) return;
		}

		sound.playMisskeySfx('reaction');
		haptic();

		if (mock) {
			if (ownChip) emit('reactionToggled', reaction, (props.count + 1));
			return;
		}

		misskeyApi('notes/reactions/create', {
			noteId: props.noteId,
			reaction: reaction,
		}).then(() => {
			const emoji = customEmojisMap.get(reactionEmojiName);
			if (emoji == null && getUnicodeEmojiOrNull(reaction) == null) {
				return;
			}

			noteEvents.emit(`reacted:${props.noteId}`, {
				userId: me.id,
				reaction: reaction,
				emoji: emoji,
			});
		});
		// TODO: 上位コンポーネントでやる
		//if (props.note.text && props.note.text.length > 100 && (Date.now() - new Date(props.note.createdAt).getTime() < 1000 * 3)) {
		//	claimAchievement('reactWithoutRead');
		//}
	}
}

// mk-go: 長押しから呼ぶときのために anchorElement を受けられるようにした (#2932)。
// **`ev.currentTarget` は setTimeout 越しでは null になる** (dispatch が終わると
// 消える) ので、長押し側は要素を明示で渡す。右クリックからの呼び出しは従来どおり。
async function menu(ev: PointerEvent | null, anchorElement?: HTMLElement) {
	let menuItems: MenuItem[] = [];

	if (isLocalCustomEmoji.value) {
		menuItems.push({
			text: i18n.ts.info,
			icon: 'ti ti-info-circle',
			action: async () => {
				const { dispose } = os.popup(MkCustomEmojiDetailedDialog, {
					emoji: await misskeyApiGet('emoji', {
						name: emojiName.value,
					}),
				}, {
					closed: () => dispose(),
				});
			},
		});
	}

	// mk-go: リモート絵文字をその場からインポートする (#2698)。**本文中の絵文字
	// (`MkCustomEmoji`) と同じモーダルを出す。** CherryPick はリアクションからだけ
	// endpoint を直接叩いていて挙動が揃っていないが、そこは踏襲しない。
	//
	// リモートのカスタム絵文字は `:name@host:` の形。ローカルは `@.` を含む
	// (`isLocalCustomEmojiReaction`)。Unicode 絵文字はコロンで始まらない。
	//
	// **権限が無い人には「申請」を出す (#2935)。** 条件は同じで押した先だけが
	// 違う。**本文とリアクションの両方に出す** — #2698 が「両方から呼ぶので
	// ここに集約する」と揃えた経緯があり、片方だけだと「リアクションからは
	// 頼めない」という気付きにくい非対称になる。
	if (props.reaction.startsWith(':') && !isLocalCustomEmoji.value && !hasLocalEmojiWithSameName(emojiName.value) && $i != null) {
		// リアクションの `emojiName` は `name@host` 形式。
		const at = emojiName.value.lastIndexOf('@');
		const canImport = canImportRemoteEmoji();
		// policies は mk-go 独自キーを含むので型を外して読む。
		const canRequest = ($i.policies as Record<string, unknown>).canRequestCustomEmojis === true;

		if (at > 0 && canImport) {
			menuItems.push({
				text: i18n.ts.import,
				icon: 'ti ti-plus',
				action: () => {
					importRemoteEmoji(emojiName.value.slice(0, at), emojiName.value.slice(at + 1));
				},
			});
			// **取り込んだ絵文字でそのままリアクションする (#3187)。** 「この絵文字で
			// リアクションしたいから取り込む」が大半なので、取り込み後にピッカーを
			// 開き直させない。送るのは確定した名前 (取り込み時に直せる) の `:name@.:`。
			// **mock (チュートリアル) では出さない。** 右クリックのメニューは mock でも
			// 開くが、mock の sendReaction は API を呼ばないので、取り込むだけで
			// リアクションされないまま終わる。
			if (!mock) {
				menuItems.push({
					text: i18n.ts._mkgoEmoji.importAndReact,
					icon: 'ti ti-mood-plus',
					action: () => {
						importRemoteEmojiAndReact(emojiName.value.slice(0, at), emojiName.value.slice(at + 1), (reaction) => sendReaction(reaction, false));
					},
				});
			}
		} else if (at > 0 && canRequest) {
			menuItems.push({
				text: i18n.ts._emojiApplication.requestImport,
				icon: 'ti ti-mood-plus',
				action: () => {
					requestRemoteEmojiImport(emojiName.value.slice(0, at), emojiName.value.slice(at + 1));
				},
			});
		}
	}

	if (isEmojiMuted(props.reaction).value) {
		menuItems.push({
			text: i18n.ts.emojiUnmute,
			icon: 'ti ti-mood-smile',
			action: () => {
				os.confirm({
					type: 'question',
					title: i18n.tsx.unmuteX({ x: isLocalCustomEmoji.value ? `:${emojiName.value}:` : props.reaction }),
				}).then(({ canceled }) => {
					if (canceled) return;
					unmuteEmoji(props.reaction);
				});
			},
		});
	} else {
		menuItems.push({
			text: i18n.ts.emojiMute,
			icon: 'ti ti-mood-off',
			action: () => {
				os.confirm({
					type: 'question',
					title: i18n.tsx.muteX({ x: isLocalCustomEmoji.value ? `:${emojiName.value}:` : props.reaction }),
				}).then(({ canceled }) => {
					if (canceled) return;
					muteEmoji(props.reaction);
				});
			},
		});
	}

	if (canReact.value) {
		menuItems.push({
			text: i18n.ts.addToEmojiPalette,
			icon: 'ti ti-palette',
			action: () => {
				// mk-go: 相乗りできるものは**ローカルの裸の名前**を入れる (#2697)。
				// `props.reaction` を入れると `:foo@host:` が永続化され、パレットから
				// 押したときだけリモートのキーを送る (mk-go は受理して合算するが、
				// 純正 TS の `isCustomEmojiRegexp` は任意 host を受けず ❤ に落ちる)。
				// `MkCustomEmoji` も `:${props.name}:` の裸の形を入れている。
				addToEmojiPalette(isLocalCustomEmoji.value || localAlternative.value != null
					? `:${bareEmojiName(emojiName.value)}:`
					: props.reaction);
			},
		});
	}

	os.popupMenu(menuItems, anchorElement ?? ev?.currentTarget ?? ev?.target);
}

function anime() {
	if (window.document.hidden || !prefer.s.animation || buttonEl.value == null) return;

	const rect = buttonEl.value.getBoundingClientRect();
	const x = rect.left + 16;
	const y = rect.top + (buttonEl.value.offsetHeight / 2);
	const { dispose } = os.popup(MkReactionEffect, { reaction: props.reaction, x, y }, {
		end: () => dispose(),
	});
}

watch(() => props.count, (newCount, oldCount) => {
	if (oldCount < newCount) anime();
});

// mk-go: 長押しでもメニューを開く (#2932)。**iOS Safari は button の長押しで
// contextmenu を発火しない**ので、`@contextmenu` だけだとリモート絵文字の
// インポート導線 (#2698) に iOS から到達できない。タップは `toggleReaction()` に
// 取られているため代わりの入口が無い。
//
// **後続の click は bindLongPress が握り潰す。** 潰さないと、メニューを開いた指を
// 離した瞬間にリアクションが付け外しされる。
let disposeLongPress: (() => void) | null = null;

onMounted(() => {
	if (!props.isInitial) anime();

	if (!mock && buttonEl.value != null) {
		disposeLongPress = bindLongPress(buttonEl.value, () => menu(null, buttonEl.value ?? undefined));
	}
});

onBeforeUnmount(() => {
	disposeLongPress?.();
	disposeLongPress = null;
});

if (!mock) {
	useTooltip(buttonEl, async (showing) => {
		if (buttonEl.value == null) return;

		const reactions = await misskeyApi('notes/reactions', {
			noteId: props.noteId,
			type: props.reaction,
			limit: 10,
		});

		const users = reactions.map(x => x.user);

		const { dispose } = os.popup(XDetails, {
			showing,
			reaction: props.reaction,
			users,
			count: props.count,
			anchorElement: buttonEl.value,
		}, {
			closed: () => dispose(),
		});
	}, 100);
}
</script>

<style lang="scss" module>
.root {
	display: inline-flex;
	height: 42px;
	// mk-go: 長押しでメニューを開く (#2932)。iOS は img を含む要素の長押しで
	// 「画像を保存」の吹き出しを出すので止める。**user-select は足さない** —
	// style.scss の html:not(.forceSelectableAll) から既に継承されており、
	// 足すと「全てのテキスト要素を選択可能にする」設定だけを打ち消す。
	-webkit-touch-callout: none;
	padding: 0 6px;
	font-size: 1.5em;
	border-radius: 6px;
	align-items: center;
	justify-content: center;

	&.canToggle {
		background: var(--MI_THEME-buttonBg);

		&:hover {
			background: rgba(0, 0, 0, 0.1);
		}
	}

	&:not(.canToggle) {
		cursor: default;
	}

	&.small {
		height: 32px;
		font-size: 1em;
		border-radius: 4px;

		> .count {
			font-size: 0.9em;
			line-height: 32px;
		}
	}

	&.large {
		height: 52px;
		font-size: 2em;
		border-radius: 8px;

		> .count {
			font-size: 0.6em;
			line-height: 52px;
		}
	}

	&.reacted, &.reacted:hover {
		background: var(--MI_THEME-accentedBg);
		color: var(--MI_THEME-accent);
		box-shadow: 0 0 0 1px var(--MI_THEME-accent) inset;

		> .count {
			color: var(--MI_THEME-accent);
		}

		> .icon {
			filter: drop-shadow(0 0 2px rgba(0, 0, 0, 0.5));
		}
	}
}

.limitWidth {
	max-width: 70px;
	object-fit: contain;
}

.count {
	font-size: 0.7em;
	line-height: 42px;
	margin: 0 0 0 4px;
}
</style>
