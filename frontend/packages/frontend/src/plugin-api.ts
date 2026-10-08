/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * Public API for mk-go server plugins (mk-go #2479).
 *
 * サーバープラグインはビルド時に取り込まれる。運営者が `plugins/` に置いた
 * ものを mk-go が Go と TypeScript の両方まとめてビルドに含める。
 *
 * # 内部コンポーネントを直接使わないこと
 *
 * プラグインは**このモジュールだけ**を import する。`MkNote` のような内部
 * コンポーネントを直接使うと、upstream Misskey のリファクタで壊れる。
 * ここに無いものが必要になったら、mk-go 側に追加を要求すること
 * (公開面を広げるのは意図的な判断として扱う)。
 *
 * # AiScript プラグインとの違い
 *
 * AiScript 版 (`plugin.ts`) は**利用者が自分で入れる**もので、サンドボックス
 * された環境で動く。こちらは**運営者がビルドに含める**もので、サンドボックスは
 * 無く、利用者のセッションで任意の JS として動く。信頼できる作者のものだけを
 * 組み込むこと。
 */

import { defineAsyncComponent } from 'vue';
import type { Component } from 'vue';
import type { entities } from 'misskey-js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { $i } from '@/i.js';

/*
 * Misskey のコンポーネントを再公開する。
 *
 * プラグインは同じバンドルに入るので技術的には何でも import できるが、
 * **ここに出ているものだけが「壊さないと約束する範囲」**。これ以外を使うと、
 * 上流のリファクタで黙って壊れる。
 *
 * 追加してほしいものがあれば mk-go 側に要求すること (公開面を広げるのは
 * 意図的な判断として扱う)。
 */
export { default as MkInput } from '@/components/MkInput.vue';
export { default as MkButton } from '@/components/MkButton.vue';
export { default as MkFolder } from '@/components/MkFolder.vue';
export { default as MkLoading } from '@/components/global/MkLoading.vue';
export { default as MkSelect } from '@/components/MkSelect.vue';
export { default as MkSwitch } from '@/components/MkSwitch.vue';
export { default as MkAvatar } from '@/components/global/MkAvatar.vue';
export { default as MkUserName } from '@/components/global/MkUserName.vue';
export { default as MkTime } from '@/components/global/MkTime.vue';
export { default as PageWithHeader } from '@/components/global/PageWithHeader.vue';
/**
 * サーバープラグインの秘密の値 (API キーなど) の入力欄 (Elythia #3470)。
 * `plugin` にプラグイン名を渡すと、`Definition.Secrets` で宣言した値の入力欄を出す。
 * 値は書き込み専用で、保存後は「設定済み」と末尾 4 文字しか出ない。保存・削除は
 * 管理者だけができる。コントロールパネルのサーバープラグイン一覧にも同じものが出る
 * ので、独自の管理画面に置くのは任意。
 *
 * 遅延読み込みにする。この部品はダイアログ (`@/os.js`) を使うので、そのまま
 * import すると plugin-api を読むだけでルーターまで巻き込む。
 */
export const MkPluginSecrets = defineAsyncComponent(() => import('@/components/MkPluginSecrets.vue'));
export { useMkSelect } from '@/composables/use-mkselect.js';
export { definePage } from '@/page.js';

/**
 * 標準ユーザー表示コンポーネントへ渡す公開ユーザー情報。
 *
 * **SlotUser と違い、misskey-js の型をそのまま公開している。** MkAvatar /
 * MkUserName の props へそのまま渡せることを優先した。そのぶん upstream が
 * UserLite を変えると、ここで約束する形も一緒に変わる。
 */
export type PluginUser = entities.UserLite;

/**
 * 重複を除き、users/show の上限（100人）ごとに公開ユーザー情報を取得する。
 *
 * 存在しない・凍結中などで見えないユーザーは結果から黙って消えるので、結果の件数は
 * 入力以下になる (順序も入力と一致するとは限らない)。形式が不正な ID が混ざると、
 * その 100 件のまとまりごと失敗してエラーを返す。
 */
export async function getUsers(userIds: string[]): Promise<PluginUser[]> {
	const ids = [...new Set(userIds)];
	const users: PluginUser[] = [];
	for (let start = 0; start < ids.length; start += 100) {
		users.push(...await misskeyApi('users/show', { userIds: ids.slice(start, start + 100) }));
	}
	return users;
}

/**
 * Named locations a plugin can render into.
 *
 * **コンポーネント名ではなく「意味的な位置」で定義する。** upstream が
 * `MkUserInfo` を別名にしても、位置の意味は変わらないのでプラグインは壊れない。
 */
export type SlotName =
	/** ユーザーのプロフィール。表示中のユーザーが ctx.user に入る。 */
	| 'profile:info'
	/** 設定 > プロフィール。自分の設定を編集させたいときはここ。 */
	| 'settings:profile'
	/**
	 * コントロールパネル > 連合。一覧の手前に出る。
	 *
	 * **描画されることは権限の保証ではない。** ここに出るのはページが
	 * モデレーター向けだからでしかないので、プラグインは API 側で
	 * `Request.IsModerator()` を必ず見ること。
	 */
	| 'admin:federation'
	/**
	 * インスタンス情報の概要タブ。対象のホストが ctx.host に入る。
	 *
	 * `admin:federation` と同じく、**描画されることは権限の保証ではない**。
	 */
	| 'admin:instance-info'
	/**
	 * ユーザーのモデレーション画面 (コントロールパネル > ユーザー) の概要タブ。
	 * 対象のユーザーが ctx.user に入る。
	 *
	 * `profile:info` と位置は似ているが**見せる相手が違う**。あちらは公開
	 * プロフィールなので、モデレーターにだけ見せたいものをそちらへ出すと
	 * 全員に見える。裁く材料はこちらへ置くこと。
	 *
	 * `admin:federation` と同じく、**描画されることは権限の保証ではない**。
	 */
	| 'admin:user';

/** Minimal user shape handed to slots. 内部の型をそのまま渡さない。 */
export type SlotUser = {
	id: string;
	username: string;
	host: string | null;
};

export type SlotContext = {
	/** プロフィール系のスロットで、表示中のユーザー。 */
	user?: SlotUser;
	/** インスタンス系のスロットで、表示中のホスト。 */
	host?: string;
};

/**
 * Mounts a plugin's UI into `el`.
 *
 * **素の DOM 要素を渡す。** Vue のインスタンスを共有しないので、upstream の
 * Vue 更新でプラグインが壊れない。プラグイン側は Vue でも素の DOM でも好きに
 * 書ける。
 *
 * 戻り値に関数を返すと、スロットが破棄されるときに呼ばれる (後片付け用)。
 */
export type SlotMount = (el: HTMLElement, ctx: SlotContext) => void | (() => void);

/**
 * A Vue component rendered inside a slot, receiving `ctx` as its only prop.
 *
 * **Misskey のコンポーネントを使うならこちら。** ホストの Vue インスタンスと
 * アプリの provide/inject をそのまま引き継ぐので、`MkInput` などが本体と同じ
 * 見た目・挙動で動く。
 *
 * 代償として、上流が該当コンポーネントの props を変えるとプラグインが壊れる。
 * 見た目の完全一致と引き換えの追従コストとして受け入れる (mk-go #2484)。
 */
export type SlotComponent = Component;

/** A slot registration accepts either form. */
export type SlotRenderer = SlotMount | { component: SlotComponent };

/** A page a plugin adds to the router. */
export type PluginPage = {
	/**
	 * Path under the plugin's namespace. `/foo` は `/plugin/<name>/foo` になる。
	 *
	 * **名前空間を切るのは意図的。** 本体のパスと混ざると、upstream が同名の
	 * ページを足したときに衝突する。
	 */
	path: string;

	/** The component rendered for this page. */
	component: SlotComponent;

	/** ナビゲーションに項目を出すときのラベル。省くと出さない。 */
	navTitle?: string;

	/** ナビ項目のアイコン (例: 'ti ti-device-gamepad')。 */
	navIcon?: string;

	/**
	 * 管理画面として `/admin/plugin/<name>` の下に置く。
	 *
	 * 表示はモデレーター以上に限られるが、**それは UI の都合でしかない**。
	 * バックエンドは `Request.IsModerator()` で必ず自分で守ること。
	 */
	admin?: boolean;
};

export type PluginHost = {
	/** プラグイン名 (mk-go 側の Definition.Name と同じ)。 */
	readonly name: string;

	/** ログイン中のユーザー。未ログインなら null。 */
	readonly me: SlotUser | null;

	/**
	 * Renders into a named slot.
	 *
	 * 同じスロットに複数のプラグインが登録できる。描画順は
	 * プラグイン名の昇順で固定する。
	 */
	slot(name: SlotName, renderer: SlotRenderer): void;

	/**
	 * Calls an mk-go API endpoint.
	 *
	 * 自分のバックエンド側は `plugin/<name>/<path>` で呼べる。
	 * 認証は利用者のセッションがそのまま使われる。
	 */
	api<T = unknown>(endpoint: string, params?: Record<string, unknown>): Promise<T>;
};

export type PluginDefinition = {
	name: string;

	/**
	 * Pages this plugin adds to the router.
	 *
	 * **`setup` ではなくここで宣言する。** ルーターはモジュール読み込み時に
	 * 現在の URL を解決するので、`setup` で登録すると**直接アクセスが 404 に
	 * なる** (画面遷移では動くので気付きにくい)。宣言なら読み込み順に依存
	 * しない。
	 */
	pages?: PluginPage[];

	setup: (host: PluginHost) => void | Promise<void>;
};

/**
 * Declares a plugin. プラグインはこれを default export する。
 */
export function definePlugin(def: PluginDefinition): PluginDefinition {
	return def;
}

export type Registration = { plugin: string; renderer: SlotRenderer };

/** A declared page, with its resolved absolute path. */
export type PageRegistration = PluginPage & { plugin: string; fullPath: string };

const registry = new Map<SlotName, Registration[]>();

/**
 * Collects the pages declared by the given plugins.
 *
 * ルーターとナビが**モジュール読み込み時**に呼ぶ。`setup` の実行を待たないので、
 * 直接 URL を開いても解決できる。
 */
export function collectPages(plugins: PluginDefinition[], admin: boolean): PageRegistration[] {
	const out: PageRegistration[] = [];
	for (const p of plugins) {
		for (const page of p.pages ?? []) {
			if ((page.admin ?? false) !== admin) continue;
			out.push({ ...page, plugin: p.name, fullPath: pagePath(p.name, page.path) });
		}
	}
	return out;
}

/** Returns the mounts registered for a slot, ordered by plugin name. */
export function slotMounts(name: SlotName): Registration[] {
	return registry.get(name) ?? [];
}

function toSlotUser(v: { id: string; username: string; host: string | null } | null): SlotUser | null {
	if (v == null) return null;
	return { id: v.id, username: v.username, host: v.host };
}

/** pagePath namespaces a plugin's path so it cannot collide with mk-go's own. */
function pagePath(name: string, path: string): string {
	const rest = path === '/' ? '' : path;
	return `/plugin/${name}${rest}`;
}

function buildHost(name: string): PluginHost {
	return {
		name,
		me: toSlotUser($i),
		slot(slotName, renderer) {
			const list = registry.get(slotName) ?? [];
			list.push({ plugin: name, renderer });
			// 描画順をプラグイン名で固定する。登録順に依存させると、ビルドの
			// 都合で並びが変わる。
			list.sort((a, b) => a.plugin.localeCompare(b.plugin));
			registry.set(slotName, list);
		},
		api(endpoint, params) {
			// misskeyApi の型は既知のエンドポイント集合に閉じているが、
			// プラグインのエンドポイントはそこに無い。ここで境界を吸収する。
			return misskeyApi(endpoint as never, (params ?? {}) as never) as never;
		},
	};
}

/**
 * Launches every server plugin bundled into this build.
 *
 * **1 つが失敗しても他を止めない。** プラグインの不具合でクライアント全体が
 * 起動しなくなる方が損害が大きい。
 */
export async function launchServerPlugins(plugins: PluginDefinition[]): Promise<void> {
	for (const p of plugins) {
		try {
			await p.setup(buildHost(p.name));
		} catch (err) {
			console.error(`[plugin:${p.name}] setup に失敗しました`, err);
		}
	}
}

/** Test hook: clears registered slots. */
export function _resetSlotsForTest(): void {
	registry.clear();
}
