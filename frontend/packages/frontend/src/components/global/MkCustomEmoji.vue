<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<img
	v-if="shouldMute"
	:class="[$style.root, { [$style.normal]: normal, [$style.noStyle]: noStyle }]"
	src="/client-assets/unknown.png"
	:title="alt"
	draggable="false"
	style="-webkit-user-drag: none;"
	@click="onClick"
/>
<img
	v-else-if="errored && fallbackToImage"
	:class="[$style.root, { [$style.normal]: normal, [$style.noStyle]: noStyle }]"
	src="/client-assets/dummy.png"
	:title="alt"
	draggable="false"
	style="-webkit-user-drag: none;"
/>
<span v-else-if="errored">:{{ customEmojiName }}:</span>
<img
	v-else
	:class="[$style.root, { [$style.normal]: normal, [$style.noStyle]: noStyle }]"
	:src="url"
	:alt="alt"
	:title="alt"
	decoding="async"
	draggable="false"
	@error="errored = true"
	@load="errored = false"
	@click="onClick"
/>
</template>

<script lang="ts" setup>
import { computed, defineAsyncComponent, inject, ref } from 'vue';
import { normalizeCustomEmojiName, isLocalCustomEmojiName, getCustomEmojiImagePath } from '@@/js/emoji-name.js';
import type { MenuItem } from '@/types/menu.js';
import { getProxiedImageUrl, getStaticImageUrl } from '@/utility/media-proxy.js';
import { customEmojisMap } from '@/custom-emojis.js';
import * as os from '@/os.js';
import { misskeyApi, misskeyApiGet } from '@/utility/misskey-api.js';
import { copyToClipboard } from '@/utility/copy-to-clipboard.js';
import { importRemoteEmoji, importRemoteEmojiAndReact, canImportRemoteEmoji, hasLocalEmojiWithSameName } from '@/utility/import-remote-emoji.js';
import { requestRemoteEmojiImport } from '@/utility/request-remote-emoji.js';
import { i18n } from '@/i18n.js';
import MkCustomEmojiDetailedDialog from '@/components/MkCustomEmojiDetailedDialog.vue';
import { $i } from '@/i.js';
import { prefer } from '@/preferences.js';
import { DI } from '@/di.js';
import { makeEmojiMuteKey, mute as muteEmoji, unmute as unmuteEmoji, checkMuted as checkEmojiMuted } from '@/utility/emoji-mute';
import { addToEmojiPalette } from '@/utility/emoji-palette.js';

const props = defineProps<{
	name: string;
	normal?: boolean;
	noStyle?: boolean;
	host?: string | null;
	url?: string;
	useOriginalSize?: boolean;
	menu?: boolean;
	menuReaction?: boolean;
	fallbackToImage?: boolean;
	ignoreMuted?: boolean;
}>();

const react = inject(DI.mfmEmojiReactCallback, null);

const customEmojiName = computed(() => normalizeCustomEmojiName(props.name));
const isLocal = computed(() => isLocalCustomEmojiName(customEmojiName.value, props.host));
const emojiCodeToMute = makeEmojiMuteKey(props);
const isMuted = checkEmojiMuted(emojiCodeToMute);
const shouldMute = computed(() => !props.ignoreMuted && isMuted.value);

const rawUrl = computed(() => {
	if (props.url) {
		return props.url;
	}
	if (isLocal.value) {
		return customEmojisMap.get(customEmojiName.value)?.url ?? null;
	}
	return getCustomEmojiImagePath(customEmojiName.value, props.host);
});

const url = computed(() => {
	if (rawUrl.value == null) return undefined;

	const proxied =
		(rawUrl.value.startsWith('/emoji/') || (props.useOriginalSize && isLocal.value))
			? rawUrl.value
			: getProxiedImageUrl(
				rawUrl.value,
				props.useOriginalSize ? undefined : 'emoji',
				false,
				true,
			);
	return prefer.s.disableShowingAnimatedImages
		? getStaticImageUrl(proxied)
		: proxied;
});

const alt = computed(() => `:${customEmojiName.value}:`);
const errored = ref(url.value == null);

function onClick(ev: PointerEvent) {
	if (props.menu) {
		const menuItems: MenuItem[] = [];

		menuItems.push({
			type: 'label',
			text: `:${props.name}:`,
		});

		if (isLocal.value) {
			menuItems.push({
				text: i18n.ts.copy,
				icon: 'ti ti-copy',
				action: () => {
					copyToClipboard(`:${props.name}:`);
				},
			});
		}

		// **ローカルの絵文字だけ。** リモートの絵文字を `:name:` で送ると、ローカルの
		// 同名 (あれば) か ❤ に解決されて、押した絵文字とは別のものになる (#3187 で
		// MkMfm がリモートにも menuReaction を渡すようになった)。
		if (isLocal.value && props.menuReaction && react) {
			menuItems.push({
				text: i18n.ts.doReaction,
				icon: 'ti ti-plus',
				action: () => {
					react(`:${props.name}:`);
				},
			});
		}

		// mk-go (#3187): 本文中のリモート絵文字に**同名のローカル絵文字があるなら**、
		// それでリアクションする。リアクションのチップの相乗り (#2697) と同じ条件
		// (設定 reactableRemoteReactionEnabled) で出す。送るのは `:name@.:`
		// (`localAlternativeReaction` と同じ理由で、返ってくる myReaction と揃える)。
		if (!isLocal.value && props.menuReaction && react && prefer.s.reactableRemoteReactionEnabled && hasLocalEmojiWithSameName(customEmojiName.value)) {
			menuItems.push({
				text: i18n.ts.doReaction,
				icon: 'ti ti-plus',
				action: () => {
					react(`:${customEmojiName.value}@.:`);
				},
			});
		}

		if (isLocal.value) {
			menuItems.push({
				type: 'divider',
			}, {
				text: i18n.ts.info,
				icon: 'ti ti-info-circle',
				action: async () => {
					const { dispose } = os.popup(MkCustomEmojiDetailedDialog, {
						emoji: await misskeyApiGet('emoji', {
							name: customEmojiName.value,
						}),
					}, {
						closed: () => dispose(),
					});
				},
			});
		}

		// mk-go: リモート絵文字をその場からインポートする (#2698)。管理画面を
		// 開かなくても、見つけた絵文字をすぐ取り込めるようにするのが目的。
		// **ローカルには出さない** (既に手元にある) し、権限が無ければ出さない。
		//
		// **同名のローカル絵文字が既にあるときも出さない (#2903)。** 押しても
		// admin/emoji/copy が重複で弾くだけで、押してみるまで分からなかった。
		// customEmojisMap は裸の名前がキー。
		//
		// **権限が無い人には「申請」を出す (#2935)。** 条件 (ローカルでない /
		// 同名が無い / ログイン済み) は同じで、**押した先が違うだけ**。権限で
		// 導線ごと消すと、欲しい絵文字を見つけても頼む手段が無い。
		if (!isLocal.value && !hasLocalEmojiWithSameName(customEmojiName.value) && $i != null) {
			const canImport = canImportRemoteEmoji();
			// policies は mk-go 独自キーを含むので型を外して読む。
			const canRequest = ($i.policies as Record<string, unknown>).canRequestCustomEmojis === true;

			if (canImport) {
				menuItems.push({
					type: 'divider',
				}, {
					text: i18n.ts.import,
					icon: 'ti ti-plus',
					action: () => {
						// **`name` と `host` は別の prop。** `customEmojiName` は
						// ホスト無しの裸の名前なので、ここで `name@host` を組もうとすると
						// 何も起きない (`MkMfm` は host を別に渡す)。
						importRemoteEmoji(customEmojiName.value, props.host);
					},
				});
				// 取り込んだ絵文字でそのままリアクションする (#3187)。リアクションできる
				// 場所 (menuReaction と react の注入がある = ノートの本文と CW) でだけ出す。
				// チャットの本文は投稿者を MkMfm に渡さないのでリモートの分岐に来ない。
				if (props.menuReaction && react) {
					menuItems.push({
						text: i18n.ts._mkgoEmoji.importAndReact,
						icon: 'ti ti-mood-plus',
						action: () => {
							importRemoteEmojiAndReact(customEmojiName.value, props.host, react);
						},
					});
				}
			} else if (canRequest) {
				menuItems.push({
					type: 'divider',
				}, {
					text: i18n.ts._emojiApplication.requestImport,
					icon: 'ti ti-mood-plus',
					action: () => {
						requestRemoteEmojiImport(customEmojiName.value, props.host);
					},
				});
			}
		}

		if (isMuted.value) {
			menuItems.push({
				text: i18n.ts.emojiUnmute,
				icon: 'ti ti-mood-smile',
				action: async () => {
					await unmute();
				},
			});
		} else {
			menuItems.push({
				text: i18n.ts.emojiMute,
				icon: 'ti ti-mood-off',
				action: async () => {
					await mute();
				},
			});
		}

		if (isLocal.value) {
			menuItems.push({
				text: i18n.ts.addToEmojiPalette,
				icon: 'ti ti-palette',
				action: () => {
					addToEmojiPalette(`:${props.name}:`);
				},
			});
		}

		if (($i?.isModerator ?? $i?.isAdmin) && isLocal.value) {
			menuItems.push({
				type: 'divider',
			}, {
				text: i18n.ts.edit,
				icon: 'ti ti-pencil',
				action: async () => {
					await edit(props.name);
				},
			});
		}

		os.popupMenu(menuItems, ev.currentTarget ?? ev.target);
	}
}

async function edit(name: string) {
	const emoji = await misskeyApi('emoji', {
		name: name,
	});
	const { dispose } = await os.popupAsyncWithDialog(import('@/pages/emoji-edit-dialog.vue').then(x => x.default), {
		emoji: emoji,
	}, {
		closed: () => dispose(),
	});
}

function mute() {
	const titleEmojiName = isLocal.value
		? `:${customEmojiName.value}:`
		: emojiCodeToMute;
	os.confirm({
		type: 'question',
		title: i18n.tsx.muteX({ x: titleEmojiName }),
	}).then(({ canceled }) => {
		if (canceled) {
			return;
		}
		muteEmoji(emojiCodeToMute);
	});
}

function unmute() {
	const titleEmojiName = isLocal.value
		? `:${customEmojiName.value}:`
		: emojiCodeToMute;
	os.confirm({
		type: 'question',
		title: i18n.tsx.unmuteX({ x: titleEmojiName }),
	}).then(({ canceled }) => {
		if (canceled) {
			return;
		}
		unmuteEmoji(emojiCodeToMute);
	});
}

</script>

<style lang="scss" module>
.root {
	height: 2em;
	vertical-align: middle;
	-webkit-user-drag: none;
	transition: transform 0.2s ease;

	&:hover {
		transform: scale(1.2);
	}
}

.normal {
	height: 1.25em;
	vertical-align: -0.25em;

	&:hover {
		transform: none;
	}
}

.noStyle {
	height: auto !important;
}
</style>
