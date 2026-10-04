<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<MkA v-user-preview="canonical" :class="[$style.root, { [$style.isMe]: isMe }]" :to="url" :behavior="navigationBehavior">
	<img :class="$style.icon" :src="avatarUrl" alt="">
	<span>
		<span>@{{ username }}</span>
		<span v-if="(host != localHost)" :class="$style.host">@{{ toUnicode(host) }}</span>
	</span>
</MkA>
</template>

<script lang="ts" setup>
import { toUnicode } from 'punycode.js';
import { computed } from 'vue';
import { host as localHost } from '@@/js/config.js';
import type { MkABehavior } from '@/components/global/MkA.vue';
import { $i } from '@/i.js';
import { prefer } from '@/preferences.js';

const props = defineProps<{
	username: string;
	host: string;
	navigationBehavior?: MkABehavior;
}>();

const canonical = props.host === localHost ? `@${props.username}` : `@${props.username}@${toUnicode(props.host)}`;

const url = `/${canonical}`;

const isMe = $i && (
	`@${props.username}@${toUnicode(props.host)}`.toLowerCase() === `@${$i.username}@${toUnicode(localHost)}`.toLowerCase()
);

/**
 * Avatar URL for the mention chip (#2908).
 *
 * **`getStaticImageUrl` に通さない。** あれは `/avatar/` を知らないので
 * `<mediaProxy>/static.webp?url=<instance>/avatar/@u@h&static=1` を組み立てるが、
 * mk-go の media proxy は open proxy ではなく allowlist が DB に実在する URL だけを
 * 通すため、この URL は 403 + `max-age=86400` になる (静止画になるどころか 1 日壊れる)。
 * `/avatar/` 側が `?static=1` を受けて署名付きプロキシ URL へ 302 する。
 */
const avatarUrl = computed(() => {
	const base = `/avatar/@${props.username}@${props.host}`;
	return prefer.s.disableShowingAnimatedImages || prefer.s.dataSaver.avatar
		? `${base}?static=1`
		: base;
});
</script>

<style lang="scss" module>
.root {
	display: inline-block;
	padding: 4px 8px 4px 4px;
	border-radius: 999px;
	color: var(--MI_THEME-mention);
	background: color(from var(--MI_THEME-mention) srgb r g b / 0.1);

	&.isMe {
		color: var(--MI_THEME-mentionMe);
		background: color(from var(--MI_THEME-mentionMe) srgb r g b / 0.1);
	}
}

.icon {
	width: 1.5em;
	height: 1.5em;
	object-fit: cover;
	margin: 0 0.2em 0 0;
	vertical-align: bottom;
	border-radius: 100%;
}

.host {
	opacity: 0.5;
}
</style>
