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
				<div :class="$style.bannerName">mk-go</div>
				<div v-if="mkGoVersion" :class="$style.bannerVersion">v{{ mkGoVersion }}</div>
			</div>

			<div style="text-align: center;">
				{{ i18n.ts._aboutMkGo.about }}
			</div>

			<!--
				**バックエンドとフロントエンドを対で出す。** Go で書き直したのは
				バックエンドだけなので、2 行が並ぶだけで構成が伝わる。
			-->
			<FormSection>
				<div class="_gaps_s">
					<MkKeyValue v-if="mkGoVersion" :copy="backendVersion">
						<template #key>{{ i18n.ts._aboutMkGo.backend }}</template>
						<template #value>{{ backendVersion }}</template>
					</MkKeyValue>
					<MkKeyValue :copy="frontendVersion">
						<template #key>{{ i18n.ts._aboutMkGo.frontend }}</template>
						<template #value>{{ frontendVersion }}</template>
					</MkKeyValue>
				</div>
			</FormSection>

			<!--
				AGPL-3.0 section 13 が求める「動いているコードに対応するソース」の案内。
				**operator が申告した URL が最優先**で、mk-go をさらに改変している場合は
				そちらが正しい案内先になる。mk-go 本体 / フロントエンドへのリンクは
				about-misskey が misskey-dev を「オリジナル」として出すのと同じ位置づけ。

				**警告 (sourceCodeIsNotYetProvided) は出さない。** mk-go 本体と
				フロントエンドのリンクは常に出るので、「ソースが 1 つも案内されていない」
				状態は起こらない。upstream の about-misskey はこの 2 本を持たないので
				警告が要るが、ここで同じ文言を出すと事実に反する。
			-->
			<FormSection>
				<template #label>{{ i18n.ts.sourceCode }}</template>
				<div class="_gaps_s">
					<FormLink v-if="serverRepositoryUrl" data-testid="about-mkgo-server-source" :to="serverRepositoryUrl" external>
						<template #icon><i class="ti ti-code"></i></template>
						{{ i18n.ts._aboutMkGo.sourceCodeOfThisServer }}
					</FormLink>
					<div v-if="serverRepositoryUrl" :class="$style.caption">
						{{ i18n.ts._aboutMkGo.sourceCodeOfThisServerDescription }}
					</div>
					<FormLink v-if="serverRepositoryUrl !== MKGO_REPOSITORY_URL" data-testid="about-mkgo-upstream-source" :to="MKGO_REPOSITORY_URL" external>
						<template #icon><i class="ti ti-brand-golang"></i></template>
						{{ i18n.ts._aboutMkGo.sourceCodeOfMkGo }}
						<template #suffix>GitHub</template>
					</FormLink>
					<!--
						**フロントエンドは別リポジトリにある。** mk-go 本体だけを案内すると、
						いま表示されているこの画面のソースが案内から漏れる (AGPL 13 条が
						対象にするのは「動いているコード」全体)。
					-->
					<FormLink data-testid="about-mkgo-frontend-source" :to="MKGO_FRONTEND_REPOSITORY_URL" external>
						<template #icon><i class="ti ti-brand-vue"></i></template>
						{{ i18n.ts._aboutMkGo.sourceCodeOfFrontend }}
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
				**アバター画像は出さない。** 新規ページなので最初から外部画像を持たせる
				必要が無く、名前だけで用は足りる。about-misskey 側の外部画像は #2892 で
				CSP に 2 origin を足して表示できるようにしたので、**「CSP で落ちるから
				出せない」わけではない** (出すなら avatars.githubusercontent.com は既に
				許可済み)。
			-->
			<FormSection>
				<template #label>{{ i18n.ts._aboutMkGo.contributors }}</template>
				<div class="_gaps_s">
					<div :class="$style.contributors">
						<a
							v-for="username in contributors"
							:key="username"
							:href="`https://github.com/${username}`"
							target="_blank"
							rel="noopener"
							:class="$style.contributor"
						>@{{ username }}</a>
					</div>
					<FormLink :to="`${MKGO_REPOSITORY_URL}/graphs/contributors`" external>
						<template #icon><i class="ti ti-users"></i></template>
						{{ i18n.ts._aboutMkGo.allContributors }}
						<template #suffix>GitHub</template>
					</FormLink>
				</div>
			</FormSection>
		</div>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import { computed } from 'vue';
import { version } from '@@/js/config.js';
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
const MKGO_REPOSITORY_URL = 'https://github.com/shiroha-a/mk';

// フロントエンドの供給元。**Go で書き直したのはサーバーサイドだけ**で、この画面は
// Misskey のフロントエンドに mk-go 向けの変更を載せたもの。mk-go 本体の submodule
// (`third_party/misskey`) として辿れるが、案内としては明示的に出す。
const MKGO_FRONTEND_REPOSITORY_URL = 'https://github.com/shiroha-a/misskey-ts';

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
	mkGoFrontendVersion?: string;
};
const mkGoVersion = mkGoMeta.mkGoVersion ?? null;

// バックエンドの版。ビルド時に revision を埋めていれば短縮ハッシュを添える
// (`mk-go 1.3.0 (abc1234)`)。`go run` や build-arg を渡さない image では空に
// なるので、そのときは版だけ出す。
const backendVersion = computed(() => {
	const commit = mkGoMeta.mkGoCommit;
	return commit ? `mk-go ${mkGoVersion} (${commit})` : `mk-go ${mkGoVersion}`;
});

// フロントエンドの版。**fork のタグ (`2026.9.0-mk.3`) を出す。**
// mk-go が同梱ビルド時の `git describe` を additive に返すのでそれを使い、
// 無ければ build 時定数 (= upstream の package.json 版) に落とす。
//
// **`instance.version` は使わない。** あれは backend が名乗る互換 Misskey 版で、
// この行が示したいフロントエンドの出どころとは別物 (`about.overview.vue` の
// サーバー情報欄は逆に backend の申告値を出す。見ているものが違う)。
const frontendVersion = computed(() => `Misskey ${mkGoMeta.mkGoFrontendVersion || version}`);

// このサーバーが動かしているコードの案内先。未設定 (null / 空文字 / upstream の列
// DEFAULT のまま) なら出さない。
const serverRepositoryUrl = computed(() => {
	const url = instance.repositoryUrl;
	if (!url || url === UPSTREAM_MISSKEY_REPOSITORY_URL) return null;
	return url;
});

// mk-go 本体のコントリビューター。GitHub API を叩くと未認証の rate limit
// (60 req/h/IP) を全閲覧者で共有することになるので静的に持つ。網羅は
// 「全てのコントリビューター」リンク側が担う。
const contributors = [
	'shiroha-a',
	'nananek',
	'Misaki0331',
	'JO3QMA',
];

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

.contributors {
	display: grid;
	grid-template-columns: repeat(auto-fill, minmax(160px, 1fr));
	grid-gap: 12px;
}

.contributor {
	display: flex;
	align-items: center;
	padding: 12px;
	background: var(--MI_THEME-buttonBg);
	border-radius: 6px;

	&:hover {
		text-decoration: none;
		background: var(--MI_THEME-buttonHoverBg);
	}
}
</style>
