<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div :class="[$style.root, { yellow: instance.isNotResponding, red: instance.isBlocked, gray: instance.isSuspended, blue: instance.isSilenced }]">
	<img class="icon" :src="getInstanceIcon(instance)" alt="" loading="lazy"/>
	<div class="body">
		<span class="host">
			<span class="hostName">{{ instance.name ?? instance.host }}</span>
			<!-- 署名方式のラベルは縮めずに常に見せ、代わりに名前側を省略する。
			     host は次の行に出るので、名前が切れても情報は失われない。 -->
			<span v-if="signatureBadges.length > 0" class="badges">
				<span
					v-for="badge in signatureBadges"
					:key="badge.kind"
					class="badge"
					:class="badge.kind"
					:title="i18n.ts._signatureCapability.description"
				>{{ badge.text }}</span>
			</span>
		</span>
		<span class="sub _monospace"><b>{{ instance.host }}</b> / {{ instance.softwareName || '?' }} {{ instance.softwareVersion }}</span>
	</div>
	<MkMiniChart v-if="chartValues" class="chart" :src="chartValues"/>
</div>
</template>

<script lang="ts" setup>
import { computed, ref } from 'vue';
import * as Misskey from 'misskey-js';
import MkMiniChart from '@/components/MkMiniChart.vue';
import { misskeyApiGet } from '@/utility/misskey-api.js';
import { getProxiedImageUrlNullable } from '@/utility/media-proxy.js';
import { signatureCapabilityOf, signatureLabels } from '@/utility/signature-capability.js';
import { i18n } from '@/i18n.js';

const props = defineProps<{
	instance: Misskey.entities.FederationInstance;
}>();

// mk-go の additive field。純正 backend では常に空配列になり、バッジは出ない。
const signatureBadges = computed(() => signatureLabels(signatureCapabilityOf(props.instance)));

const chartValues = ref<number[] | null>(null);

misskeyApiGet('charts/instance', { host: props.instance.host, limit: 16 + 1, span: 'day' }).then(res => {
	// 今日のぶんの値はまだ途中の値であり、それも含めると大抵の場合前日よりも下降しているようなグラフになってしまうため今日は弾く
	res.requests.received.splice(0, 1);
	chartValues.value = res.requests.received;
});

function getInstanceIcon(instance: Misskey.entities.FederationInstance): string {
	return getProxiedImageUrlNullable(instance.iconUrl, 'preview') ?? getProxiedImageUrlNullable(instance.faviconUrl, 'preview') ?? '/client-assets/dummy.png';
}
</script>

<style lang="scss" module>
.root {
	$bodyTitleHieght: 18px;
	$bodyInfoHieght: 16px;

	display: flex;
	align-items: center;
	padding: 16px;
	background: var(--MI_THEME-panel);
	border-radius: 8px;

	> :global(.icon) {
		display: block;
		width: ($bodyTitleHieght + $bodyInfoHieght);
		height: ($bodyTitleHieght + $bodyInfoHieght);
		object-fit: cover;
		border-radius: 4px;
		margin-right: 10px;
	}

	> :global(.body) {
		flex: 1;
		overflow: hidden;
		font-size: 0.9em;
		color: var(--MI_THEME-fg);
		padding-right: 8px;

		> :global(.host) {
			display: flex;
			align-items: center;
			gap: 4px;
			width: 100%;
			line-height: $bodyTitleHieght;

			> :global(.hostName) {
				flex: 1;
				min-width: 0;
				white-space: nowrap;
				overflow: hidden;
				text-overflow: ellipsis;
			}

			> :global(.badges) {
				display: flex;
				flex-shrink: 0;
				gap: 4px;
			}

			:global(.badge) {
				font-size: 70%;
				line-height: 1.4;
				padding: 0 5px;
				border-radius: 4px;
				white-space: nowrap;

				&:global(.ed25519) {
					color: var(--MI_THEME-accent);
					background: var(--MI_THEME-accentedBg);
				}

				&:global(.ld) {
					color: var(--MI_THEME-infoFg);
					background: var(--MI_THEME-infoBg);
				}

				// RSA は全サーバー共通で情報量が無いので、存在だけ分かれば十分。
				&:global(.rsa) {
					color: var(--MI_THEME-fg);
					border: 1px solid var(--MI_THEME-divider);
					opacity: 0.6;
				}
			}
		}

		> :global(.sub) {
			display: block;
			width: 100%;
			font-size: 80%;
			opacity: 0.7;
			line-height: $bodyInfoHieght;
			white-space: nowrap;
			overflow: hidden;
			text-overflow: ellipsis;
		}
	}

	> :global(.chart) {
		height: 30px;
	}

  &:global(.blue) {
    --c: rgba(0, 42, 255, 0.15);
    background-image: linear-gradient(45deg, var(--c) 16.67%, transparent 16.67%, transparent 50%, var(--c) 50%, var(--c) 66.67%, transparent 66.67%, transparent 100%);
    background-size: 16px 16px;
  }

	&:global(.yellow) {
		--c: rgb(255 196 0 / 15%);
		background-image: linear-gradient(45deg, var(--c) 16.67%, transparent 16.67%, transparent 50%, var(--c) 50%, var(--c) 66.67%, transparent 66.67%, transparent 100%);
		background-size: 16px 16px;
	}

	&:global(.red) {
		--c: rgb(255 0 0 / 15%);
		background-image: linear-gradient(45deg, var(--c) 16.67%, transparent 16.67%, transparent 50%, var(--c) 50%, var(--c) 66.67%, transparent 66.67%, transparent 100%);
		background-size: 16px 16px;
	}

	&:global(.gray) {
		--c: var(--MI_THEME-bg);
		background-image: linear-gradient(45deg, var(--c) 16.67%, transparent 16.67%, transparent 50%, var(--c) 50%, var(--c) 66.67%, transparent 66.67%, transparent 100%);
		background-size: 16px 16px;
	}
}
</style>
