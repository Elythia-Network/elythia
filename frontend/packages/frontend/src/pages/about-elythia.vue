<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<PageWithHeader :actions="headerActions" :tabs="headerTabs">
	<div class="_spacer" style="--MI_SPACER-w: 600px; --MI_SPACER-min: 20px;">
		<div class="_gaps_m">
			<div v-panel :class="$style.banner">
				<div :class="$style.bannerName">Elythia</div>
				<div v-if="mkGoVersion" :class="$style.bannerVersion">v{{ mkGoVersion }}</div>
			</div>

			<!--
				**フロントエンドの行は持たない。** 以前はバックエンドとフロントエンドの版を
				対で出していたが、frontend を本体へ取り込んで 1 つのリポジトリ・1 つの版に
				なった (#3379)。追従している本家の版は「サーバー情報」の Misskey の欄にある。
			-->
			<FormSection v-if="mkGoVersion">
				<MkKeyValue :copy="elythiaVersion">
					<template #key>{{ i18n.ts.version }}</template>
					<template #value>{{ elythiaVersion }}</template>
				</MkKeyValue>
			</FormSection>

			<!--
				AGPL-3.0 section 13 が求める「動いているコードに対応するソース」の案内。
				**operator が申告した URL が最優先**で、mk-go をさらに改変している場合は
				そちらが正しい案内先になる。mk-go 本体へのリンクは about-misskey が
				misskey-dev を「オリジナル」として出すのと同じ位置づけ。

				**警告 (sourceCodeIsNotYetProvided) は出さない。** mk-go 本体のリポジトリは
				このサーバーの案内先か mk-go 本体のどちらかとして常に出るので、「ソースが
				1 つも案内されていない」状態は起こらない。upstream の about-misskey は
				この案内を持たないので警告が要るが、ここで同じ文言を出すと事実に反する。

				**フロントエンドの行は持たない。** 以前は fork (shiroha-a/misskey-ts) を別に
				案内していたが、#3379 で frontend を mk-go 本体へ取り込んだので、mk-go 本体の
				リンクがこの画面のソースも案内する (AGPL 13 条が対象にする「動いている
				コード」全体)。
			-->
			<FormSection>
				<template #label>{{ i18n.ts.sourceCode }}</template>
				<div class="_gaps_s">
					<FormLink v-if="serverRepositoryUrl" data-testid="about-elythia-server-source" :to="serverRepositoryUrl" external>
						<template #icon><i class="ti ti-code"></i></template>
						{{ i18n.ts._aboutMkGo.sourceCodeOfThisServer }}
					</FormLink>
					<div v-if="serverRepositoryUrl" :class="$style.caption">
						{{ i18n.ts._aboutMkGo.sourceCodeOfThisServerDescription }}
					</div>
					<FormLink v-if="serverRepositoryUrl !== MKGO_REPOSITORY_URL" data-testid="about-elythia-upstream-source" :to="MKGO_REPOSITORY_URL" external>
						<template #icon><i class="ti ti-brand-golang"></i></template>
						{{ i18n.ts._aboutMkGo.sourceCodeOfMkGo }}
						<template #suffix>GitHub</template>
					</FormLink>
					<FormLink :to="`${MKGO_REPOSITORY_URL}/blob/develop/LICENSE`" external>
						<template #icon><i class="ti ti-license"></i></template>
						{{ i18n.ts._aboutMkGo.license }}
						<template #suffix>AGPL-3.0</template>
					</FormLink>
				</div>
			</FormSection>

			<!--
				Misskey 本体への導線。**帰属の文章は置かない** — ソースコード欄が
				「フロントエンド (Misskey のフォーク)」を挙げており、AGPL が求める
				著作権表示は LICENSE と各ファイルの SPDX ヘッダーが担っている。
			-->
			<FormSection>
				<FormLink to="/about-misskey">
					<template #icon><i class="ti ti-info-circle"></i></template>
					{{ i18n.ts.aboutMisskey }}
				</FormLink>
			</FormSection>

			<!--
				**コントリビューターの一覧は持たない。** 静的な一覧は更新が追いつかず、網羅は
				GitHub 側が担う。代わりに開発の拠点 (Elythia-Network) を案内する。
			-->
			<FormSection>
				<template #label>Elythia Network</template>
				<a :href="ELYTHIA_NETWORK_URL" target="_blank" rel="noopener" :class="$style.org" data-testid="about-elythia-network">
					<img :src="elythiaIcon" alt="" :class="$style.orgIcon"/>
					<div :class="$style.orgBody">
						<div :class="$style.orgName">Elythia-Network</div>
						<div :class="$style.orgDescription">{{ i18n.ts._aboutMkGo.elythiaNetworkDescription }}</div>
					</div>
					<i class="ti ti-brand-github" :class="$style.orgGo"></i>
					<i class="ti ti-external-link" :class="$style.orgGo"></i>
				</a>
			</FormSection>
		</div>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import { computed } from 'vue';
import elythiaIcon from '/client-assets/elythia-icon.png';
import FormLink from '@/components/form/link.vue';
import FormSection from '@/components/form/section.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import { i18n } from '@/i18n.js';
import { instance } from '@/instance.js';
import { definePage } from '@/page.js';

// mk-go 本体のリポジトリ。about-misskey が misskey-dev を固定で出すのと同じ
// 位置づけで、**このサーバーが動かしているコード** (instance.repositoryUrl) とは
// 別物として並べる。backend 側は internal/config.MkGoRepositoryURL に同じ値を持ち、
// meta.repositoryUrl の既定値と nodeinfo の software.repository がそれを使う (#2700)。
const MKGO_REPOSITORY_URL = 'https://github.com/Elythia-Network/elythia';

// Elythia の開発の拠点 (GitHub の組織)。
const ELYTHIA_NETWORK_URL = 'https://github.com/Elythia-Network';

// Misskey 本体のリポジトリ。**この値が入っているのは「未設定」を意味する。**
// `meta.repositoryUrl` の列 DEFAULT が upstream 互換でこの URL になっており、
// Misskey TS が作った meta 行 (drop-in 移行してきた DB) は必ずこれを持つ。operator の
// 申告ではないので、動いているのが mk-go である以上「このサーバーのコード」として
// 案内すると誤りになる。mk-go の migration 000084 が起動時に埋め直すが、適用前や
// operator が明示設定した場合に備えてここでも弾く。
const UPSTREAM_MISSKEY_REPOSITORY_URL = 'https://github.com/misskey-dev/misskey';

// mk-go が additive に返す値 (#2274 / #2700)。純正 backend には無いので optional。
// autogen の MetaDetailed には無い field なのでここで型を広げる (autogen 再生成で消えないように)。
const mkGoMeta = instance as typeof instance & {
	mkGoVersion?: string;
	mkGoCommit?: string;
};
const mkGoVersion = mkGoMeta.mkGoVersion ?? null;

// Elythia の版。ビルド時に revision を埋めていれば短縮ハッシュを添える
// (`Elythia 1.3.0 (abc1234)`)。`go run` や build-arg を渡さない image では空に
// なるので、そのときは版だけ出す。
const elythiaVersion = computed(() => {
	const commit = mkGoMeta.mkGoCommit;
	return commit ? `Elythia ${mkGoVersion} (${commit})` : `Elythia ${mkGoVersion}`;
});

// このサーバーが動かしているコードの案内先。未設定 (null / 空文字 / upstream の列
// DEFAULT のまま) なら出さない。
const serverRepositoryUrl = computed(() => {
	const url = instance.repositoryUrl;
	if (!url || url === UPSTREAM_MISSKEY_REPOSITORY_URL) return null;
	return url;
});

const headerActions = computed(() => []);

const headerTabs = computed(() => []);

definePage(() => ({
	title: i18n.ts.aboutMkGo,
	icon: null,
}));
</script>

<style lang="scss" module>
.banner {
	border-radius: var(--MI-radius);
	text-align: center;
	padding: 32px 16px;
}

.bannerName {
	font-size: 2em;
	font-weight: bold;
}

.bannerVersion {
	opacity: 0.7;
	margin-top: 4px;
}

.caption {
	font-size: 0.85em;
	opacity: 0.7;
	padding: 0 8px;
}

.org {
	display: flex;
	align-items: center;
	gap: 14px;
	padding: 14px;
	border-radius: var(--MI-radius);
	background: var(--MI_THEME-panel);
	border: solid 1px var(--MI_THEME-divider);
	color: inherit;

	&:hover {
		text-decoration: none;
		background: var(--MI_THEME-panelHighlight);
	}
}

.orgIcon {
	flex-shrink: 0;
	width: 48px;
	height: 48px;
	border-radius: 22%;
}

.orgBody {
	flex: 1;
	min-width: 0;
}

.orgName {
	font-weight: bold;
}

.orgDescription {
	font-size: 0.85em;
	opacity: 0.75;
}

.orgGo {
	flex-shrink: 0;
	opacity: 0.6;
}
</style>
