<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	Elythia: サーバープラグインの秘密の値 (API キーなど) の入力欄 (#3470)。
	値は書き込み専用で、保存した後は「設定済み」と末尾 4 文字しか出ない。
	保存・削除できるのは管理者だけで、サーバー側でも同じ条件を見ている。
-->
<template>
<div class="_gaps_s">
	<MkLoading v-if="loading"/>
	<MkInfo v-else-if="forbidden === 'admin'">秘密の値を扱えるのは管理者だけです。</MkInfo>
	<MkInfo v-else-if="forbidden === 'token'" warn>秘密の値は、ブラウザでログインしたときだけ扱えます (アプリや API のトークンでは操作できません)。</MkInfo>
	<MkInfo v-else-if="loadError" warn>秘密の値を読み込めませんでした: {{ loadError }}</MkInfo>
	<template v-else-if="list">
		<MkInfo v-if="!list.available" warn>
			設定ファイルに pluginSecretKey が無いため、秘密の値を保存できません。鍵を設定して再起動してください。
		</MkInfo>
		<MkInfo v-if="list.secrets.length === 0">このプラグインは秘密の値を宣言していません。</MkInfo>
		<div v-for="s in list.secrets" :key="s.name" class="_gaps_s" :class="$style.secret">
			<MkInput
				v-if="s.declared"
				v-model="inputs[s.name]"
				type="password"
				autocomplete="new-password"
				:spellcheck="false"
				:disabled="!list.available || busy"
				:placeholder="s.configured ? '新しい値を入れると上書きします' : ''"
			>
				<template #label>{{ s.name }}</template>
				<template #caption>
					<span v-if="s.description">{{ s.description }}<br></span>
					{{ statusText(s) }}
				</template>
			</MkInput>
			<div v-else>
				<div :class="$style.label">{{ s.name }}（プラグインが保存した値）</div>
				<div :class="$style.caption">{{ statusText(s) }}</div>
			</div>
			<div class="_buttons">
				<MkButton
					v-if="s.declared"
					primary
					small
					:disabled="!list.available || busy || !inputs[s.name]"
					@click="save(s.name)"
				>
					<i class="ti ti-check"></i> 保存
				</MkButton>
				<MkButton
					v-if="s.configured"
					danger
					small
					:disabled="!list.available || busy"
					@click="remove(s.name)"
				>
					<i class="ti ti-trash"></i> 削除
				</MkButton>
			</div>
		</div>
	</template>
</div>
</template>

<script lang="ts" setup>
import { onMounted, reactive, ref } from 'vue';
import type * as Elythia from 'elythia-js';
import MkButton from '@/components/MkButton.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkInput from '@/components/MkInput.vue';
import MkLoading from '@/components/global/MkLoading.vue';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';

const props = defineProps<{
	/** The server plugin's name (`Definition.Name`). */
	plugin: string;
}>();

const loading = ref(true);
const busy = ref(false);
// 'admin' は管理者でない、'token' はブラウザのログイン以外の token。直し方が違うので分ける。
const forbidden = ref<'admin' | 'token' | null>(null);
const loadError = ref<string | null>(null);
const list = ref<Elythia.PluginSecretList | null>(null);
// 入力中の値。保存したら消す (画面に残さない)。
const inputs = reactive<Record<string, string>>({});

// プラグインのエンドポイントは misskey-js の型の外にあるので、境界で吸収する
// (plugin-api.ts の host.api と同じ)。
function call<T>(path: string, params: Record<string, unknown> = {}): Promise<T> {
	return misskeyApi(`plugin/${props.plugin}/_secrets${path}` as never, params as never) as never;
}

// 内部のエラーの前置き (`pluginsecret:` など) は利用者に見せない。サーバーも
// 利用者向けの文言を返すが、古い版や想定外の経路に備えてここでも落とす。
function errorMessage(err: unknown): string {
	const message = (err as { message?: string } | null)?.message ?? String(err);
	return message.replace(/^pluginsecret:\s*/, '');
}

async function load() {
	try {
		const res = await call<Elythia.PluginSecretList>('');
		// 入力口が張られていないとき (プラグインが無効、宣言が無いなど) は、本体の
		// catchall が 200 + {} を返す。空の一覧と取り違えないよう、形で見分ける。
		if (!Array.isArray(res?.secrets)) {
			throw new Error('このプラグインの秘密の値の入力口がありません (無効か、秘密の値を宣言していません)');
		}
		list.value = res;
		forbidden.value = null;
		loadError.value = null;
	} catch (err) {
		// 管理者でないとき (モデレーターなど) は 403。読み込みの失敗とは案内を分ける。
		const code = (err as { code?: string } | null)?.code;
		if (code === 'ROLE_PERMISSION_DENIED') {
			forbidden.value = 'admin';
		} else if (code === 'ACCESS_DENIED') {
			forbidden.value = 'token';
		} else {
			loadError.value = errorMessage(err);
		}
	} finally {
		loading.value = false;
	}
}

function statusText(s: Elythia.PluginSecretInfo): string {
	if (!s.configured) return '未設定';
	if (!s.readable) {
		return list.value?.available
			? '設定済みですが、今の鍵では読めません (鍵が変わっています)。入れ直してください'
			: '設定済み (鍵が無いため確認できません)';
	}
	return s.hint != null ? `設定済み (末尾 ${s.hint})` : '設定済み';
}

async function save(name: string) {
	const value = inputs[name];
	if (!value) return;
	busy.value = true;
	try {
		await call('/set', { name, value });
		inputs[name] = '';
		await load();
		os.success();
	} catch (err) {
		os.alert({ type: 'error', text: errorMessage(err) });
	} finally {
		busy.value = false;
	}
}

async function remove(name: string) {
	const { canceled } = await os.confirm({
		type: 'warning',
		text: `${name} を削除しますか? プラグインはこの値を使えなくなります。`,
	});
	if (canceled) return;
	busy.value = true;
	try {
		await call('/delete', { name });
		await load();
	} catch (err) {
		os.alert({ type: 'error', text: errorMessage(err) });
	} finally {
		busy.value = false;
	}
}

onMounted(load);
</script>

<style lang="scss" module>
.secret {
	padding-bottom: 8px;
}

.label {
	font-size: 0.85em;
	padding: 0 0 8px 0;
}

.caption {
	font-size: 0.85em;
	opacity: 0.75;
}
</style>
