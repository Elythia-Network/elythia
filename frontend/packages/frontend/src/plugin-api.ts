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
import { FILE_TYPE_BROWSERSAFE } from '@@/js/const.js';
import type { Content } from '@/components/MkLightbox.item.vue';
import { misskeyApi } from '@/utility/misskey-api.js';
import { $i } from '@/i.js';
import { instance } from '@/instance.js';

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
// ルーターやダイアログをAPIモジュールの読み込みだけで初期化しない。
export const MkA = defineAsyncComponent(() => import('@/components/global/MkA.vue'));
export const MkUrl = defineAsyncComponent(() => import('@/components/global/MkUrl.vue'));
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
/**
 * Hides sensitive or data-saver media until revealed, following the user's settings (Elythia #4191).
 *
 * 本体のメディア表示と同じ規則 (`nsfw` の設定、データセーバー、表示前の確認、強調の枠) で隠す。
 * props は `isSensitive` (必須)、`type` (`'image' | 'video' | 'audio'`、既定は `'image'`)、
 * `size` (バイト数。データセーバーが有効なときの表示に使う)、`controls` (表示した後に隠し直すボタン。既定は true)。
 *
 * **隠している間は既定の slot を描画しない。** 表示を押してから slot を描画するので、
 * 中身の画像や動画は表示されるまで取得されない。slot には隠し直す関数 `hide` を渡す。
 * 根の要素は親の幅と高さに従うので、隠している間の大きさは親で決める (縦横比を保つなら親に `aspect-ratio` を付ける)。
 * `isSensitive` か利用者の設定 (`nsfw` / データセーバー) が変わると、表示した後でも既定の状態に戻す。
 *
 * 遅延読み込みにする。判定がダイアログ (`@/os.js`) を使うため。
 */
export const MkSensitiveMedia = defineAsyncComponent(() => import('@/components/MkSensitiveMedia.vue'));
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

/** Content that can be offered to a plugin preview provider. */
export type PreviewInput =
	| { kind: 'url'; href: string }
	| {
		kind: 'file';
		file: {
			id: string;
			name: string;
			size: number;
			url: string;
			declaredMimeType: string | null;
			detectedMimeType: string | null;
		};
	};

/** Runtime state available to a preview provider after it has matched. */
export type PreviewContext = {
	detail: boolean;
	/** True only after the user explicitly requested a manual preview. */
	manual?: boolean;
	/** The current value of the provider's named preference, if any. */
	preferenceValue?: unknown;
	revealed: boolean;
	active: boolean;
	inViewport: boolean;
	source: { kind: 'note'; noteId: string } | null;
};

export type PreviewPresentation = 'automatic' | 'manual' | 'disabled';

/** A lazy preview implementation registered by a bundled server plugin. */
export type PreviewProvider<T = unknown> = {
	id: string;
	priority?: number;
	requiresDetail?: boolean;
	/** Reads an existing native preference and exposes its value to presentation/load. */
	preferenceKey?: string;
	/** Chooses automatic loading, a manual action, or no preview for this context. */
	presentation?: (input: PreviewInput, context: PreviewContext) => PreviewPresentation;
	/** Text for the manual preview action. */
	manualLabel?: string;
	loadingComponent?: Component;
	match(input: PreviewInput): boolean;
	load(input: PreviewInput, context: PreviewContext, signal: AbortSignal): Promise<T>;
	component: Component;
};

/** A provider after its plugin namespace has been applied. */
export type PreviewRegistration = {
	id: string;
	plugin: string;
	provider: PreviewProvider;
};

/** A synchronous plain-text transform for text pasted into the post form. */
export type PasteTextTransform = (text: string) => string;

type PasteTextRegistration = {
	id: string;
	plugin: string;
	transform: PasteTextTransform;
	options?: PasteTextTransformOptions;
};

/** Optional native preference and notification resolved by the post form at paste time. */
export type PasteTextTransformOptions = {
	enabledPreferenceKey?: string;
	/** Resolves localized feedback at paste time, after this transform changes the text. */
	notification?: () => string;
};

export type PasteTextTransformRuntime = {
	isPreferenceEnabled: (key: string) => boolean | undefined;
};

export type TransformedPasteText = { text: string; notification?: string };

/**
 * A per-note value the author sets from a post form button (#4199).
 *
 * 値は `notes/create` の `pluginOptions.<プラグイン名>` としてそのまま送られ、
 * バックエンドの `plugin.NoteDeliveryRegistration.Prepare` が受け取る。本体は
 * 中身を読まない。
 */
export type NoteOptionDefinition = {
	/** Icon class of the footer button (例: 'ti ti-world-share')。 */
	icon: string;
	/** Tooltip and dialog title, resolved each time so it follows the language setting. */
	label: () => string;
	/**
	 * Edits the value inside a host dialog. Receives `modelValue` and emits
	 * `update:modelValue`. 未設定のときの `modelValue` は null。
	 */
	component: SlotComponent;
	/**
	 * Whether a value asks for anything. 偽なら送らず、ボタンも強調しない。
	 * 省略すると null / undefined 以外を設定済みとみなす。
	 */
	isSet?: (value: unknown) => boolean;
};

/** A registered post form option. `plugin` is also its key in `pluginOptions`. */
export type NoteOptionRegistration = NoteOptionDefinition & { plugin: string };

/** A fetched media Blob owned by a plugin, rather than a drive file. */
export type PluginMediaItem = {
	id: string;
	type: 'image' | 'video' | 'audio';
	blob: Blob;
	width?: number | null;
	height?: number | null;
	filename?: string | null;
	alt?: string;
	isSensitive?: boolean;
	initiallyRevealed?: boolean;
};

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

	/** Registers a lazy content preview under this plugin's namespace. */
	readonly preview: {
		register<T = unknown>(provider: PreviewProvider<T>): void;
	};

	/** Synchronous plain-text transforms for text pasted into the post form. */
	readonly postForm: {
		registerPasteTransform(id: string, transform: PasteTextTransform, options?: PasteTextTransformOptions): void;
		/**
		 * Adds a footer button that sets this plugin's per-note value (#4199).
		 * 1 つのプラグインにつき 1 つまで (`pluginOptions` のキーがプラグイン名のため)。
		 */
		registerNoteOption(option: NoteOptionDefinition): void;
	};

	/**
	 * Calls an mk-go API endpoint.
	 *
	 * 自分のバックエンド側は `plugin/<name>/<path>` で呼べる。
	 * 認証は利用者のセッションがそのまま使われる。
	 */
	api<T = unknown>(endpoint: string, params?: Record<string, unknown>, options?: { signal?: AbortSignal }): Promise<T>;

	/** Fetches a Response from this plugin's root-relative API path using the host session. */
	fetch(path: string, options?: RequestInit): Promise<Response>;

	/** Opens browser-safe fetched Blobs in the host media viewer. */
	openMedia(items: PluginMediaItem[], initialId?: string): Promise<void>;

	/**
	 * Reports whether media should start hidden under the user's settings.
	 *
	 * `MkSensitiveMedia` と同じ判定 (`nsfw` の設定とデータセーバー)。部品で足りないときに使う。
	 */
	shouldHideSensitiveMedia(isSensitive: boolean): boolean;

	/**
	 * Asks for confirmation before revealing sensitive media when the user wants it.
	 *
	 * 利用者が確認を求める設定でなければ、すぐ true を返す。取り消されたら false。
	 */
	canRevealSensitiveMedia(isSensitive: boolean): Promise<boolean>;
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
const previewRegistry = new Map<string, PreviewRegistration>();
const pasteTransformRegistry = new Map<string, PasteTextRegistration>();
const noteOptionRegistry = new Map<string, NoteOptionRegistration>();

function registerNoteOption(plugin: string, option: NoteOptionDefinition): void {
	if (noteOptionRegistry.has(plugin)) {
		throw new Error(`Post form note option is already registered: ${plugin}`);
	}
	if (typeof option.label !== 'function' || option.component == null) {
		throw new Error(`Post form note option needs a label and a component: ${plugin}`);
	}
	noteOptionRegistry.set(plugin, { ...option, plugin });
}

/**
 * Returns the registered post form options the server accepts, ordered by
 * plugin name.
 *
 * **サーバーで有効なものだけを返す (#4202)。** frontend はビルドに含めた全部の
 * プラグインを起動するが、サーバーで無効にしたプラグイン (`enabled: false`) は
 * `pluginOptions` を受け取らない。ボタンを出すと、指定した投稿が毎回
 * `INVALID_PARAM` になり、端末に残る書きかけの投稿からも指定が戻る。受け取る
 * プラグインは meta の `noteDeliveryPlugins` で知る。古いサーバーや端末に残った
 * 古い meta で欠けているときは、どれも出さない。
 */
export function noteOptions(): NoteOptionRegistration[] {
	const accepted = instance.noteDeliveryPlugins ?? [];
	return [...noteOptionRegistry.values()]
		.filter(o => accepted.includes(o.plugin))
		.sort((a, b) => a.plugin < b.plugin ? -1 : a.plugin > b.plugin ? 1 : 0);
}

/**
 * Returns the option's label, or the plugin name when `label()` throws, so a
 * broken plugin cannot keep the post form from rendering (#4202).
 */
export function noteOptionLabel(option: NoteOptionRegistration): string {
	try {
		const label = option.label();
		return typeof label === 'string' ? label : option.plugin;
	} catch {
		return option.plugin;
	}
}

/** Reports whether a post form option value asks for anything. 判定が例外を投げたら、送る側 (設定済み) に倒す。 */
export function isNoteOptionSet(option: NoteOptionRegistration, value: unknown): boolean {
	if (option.isSet == null) return value != null;
	try {
		return option.isSet(value);
	} catch {
		return value != null;
	}
}

function registerPreviewProvider<T>(plugin: string, provider: PreviewProvider<T>): void {
	if (provider.id.length === 0 || provider.id.includes(':')) {
		throw new Error(`Invalid preview provider ID: ${provider.id}`);
	}

	const id = `${plugin}:${provider.id}`;
	if (previewRegistry.has(id)) {
		throw new Error(`Preview provider is already registered: ${id}`);
	}

	previewRegistry.set(id, {
		id,
		plugin,
		provider: provider as PreviewProvider,
	});
}

function registerPasteTextTransform(plugin: string, id: string, transform: PasteTextTransform, options?: PasteTextTransformOptions): void {
	if (typeof id !== 'string' || id.length === 0 || id.includes(':')) {
		throw new Error(`Invalid post form paste transform ID: ${id}`);
	}
	if (typeof transform !== 'function') {
		throw new Error(`Post form paste transform must be a function: ${plugin}:${id}`);
	}

	const namespacedID = `${plugin}:${id}`;
	if (pasteTransformRegistry.has(namespacedID)) {
		throw new Error(`Post form paste transform is already registered: ${namespacedID}`);
	}

	pasteTransformRegistry.set(namespacedID, { id: namespacedID, plugin, transform, options });
}

/** Applies registered transforms in stable namespaced-ID order. */
export function transformPastedText(text: string, runtime?: PasteTextTransformRuntime): TransformedPasteText {
	let current = text;
	let notification: string | undefined;
	const registrations = [...pasteTransformRegistry.values()].sort((a, b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
	for (const registration of registrations) {
		const preferenceKey = registration.options?.enabledPreferenceKey;
		if (preferenceKey && runtime) {
			try {
				if (runtime.isPreferenceEnabled(preferenceKey) === false) continue;
			} catch {
				// An unavailable preference uses the declared default (enabled).
			}
		}
		const before = current;
		try {
			const transformed: unknown = registration.transform(current);
			if (typeof transformed !== 'string') {
				// A Promise is not supported, but consume a rejected one so a plugin
				// cannot create an unhandled rejection while the host keeps sync paste.
				try {
					if (transformed !== null && (typeof transformed === 'object' || typeof transformed === 'function') && typeof (transformed as { then?: unknown }).then === 'function') {
						void Promise.resolve(transformed).catch(() => {});
					}
				} catch {
					// Ignore invalid thenables without exposing clipboard data or errors.
				}
				if (_DEV_) console.warn(`[plugin:post-form-paste:${registration.id}] transform returned a non-string; keeping previous text`);
				continue;
			}
			current = transformed;
			if (current !== before && notification == null && registration.options?.notification != null) {
				try {
					const resolvedNotification = registration.options.notification();
					if (typeof resolvedNotification === 'string') notification = resolvedNotification;
				} catch {
					// Feedback is optional; never interrupt the user's paste.
				}
			}
		} catch {
			// Never log the exception: a plugin could include the pasted text in it.
			if (_DEV_) console.warn(`[plugin:post-form-paste:${registration.id}] transform failed; keeping previous text`);
		}
	}
	return { text: current, ...(notification ? { notification } : {}) };
}

/** Resolves one deterministic winner without performing provider I/O. */
export function resolvePreviewProvider(input: PreviewInput): PreviewRegistration | null {
	const matches: PreviewRegistration[] = [];
	for (const registration of previewRegistry.values()) {
		try {
			if (registration.provider.match(input)) matches.push(registration);
		} catch (err) {
			if (_DEV_) console.warn(`[preview:${registration.id}] matcher failed`, err);
		}
	}

	matches.sort((a, b) => {
		const priority = (b.provider.priority ?? 0) - (a.provider.priority ?? 0);
		return priority !== 0 ? priority : a.id < b.id ? -1 : 1;
	});

	if (matches.length > 1) {
		if (_DEV_) console.warn(`[preview] multiple providers matched; selected ${matches[0].id}; candidates: ${matches.map(match => match.id).join(', ')}`);
	}

	return matches[0] ?? null;
}

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

type SensitiveMediaJudge = Pick<typeof import('@/utility/sensitive-file.js'), 'shouldHideSensitiveMediaByDefault' | 'canRevealSensitiveMedia'>;

// 判定のモジュールを読めなかったときの代わり。表示してしまう側には倒さない。
const fallbackSensitiveMediaJudge: SensitiveMediaJudge = {
	shouldHideSensitiveMediaByDefault: () => true,
	canRevealSensitiveMedia: async isSensitive => !isSensitive,
};

const importSensitiveMediaJudge = (): Promise<SensitiveMediaJudge> => import('@/utility/sensitive-file.js');
let sensitiveMediaJudgeImporter = importSensitiveMediaJudge;

async function loadSensitiveMediaJudge(): Promise<SensitiveMediaJudge> {
	try {
		return await sensitiveMediaJudgeImporter();
	} catch (err) {
		console.error('[plugin] failed to load the sensitive media settings', err);
		return fallbackSensitiveMediaJudge;
	}
}

function buildHost(name: string, sensitive: SensitiveMediaJudge): PluginHost {
	const pluginAPIPrefix = `/api/plugin/${name}/`;
	return {
		name,
		me: toSlotUser($i),
		preview: {
			register(provider) {
				registerPreviewProvider(name, provider);
			},
		},
		postForm: {
			registerPasteTransform(id, transform, options) {
				registerPasteTextTransform(name, id, transform, options);
			},
			registerNoteOption(option) {
				registerNoteOption(name, option);
			},
		},
		slot(slotName, renderer) {
			const list = registry.get(slotName) ?? [];
			list.push({ plugin: name, renderer });
			// 描画順をプラグイン名で固定する。登録順に依存させると、ビルドの
			// 都合で並びが変わる。
			list.sort((a, b) => a.plugin.localeCompare(b.plugin));
			registry.set(slotName, list);
		},
		api(endpoint, params, options) {
			// misskeyApi の型は既知のエンドポイント集合に閉じているが、
			// プラグインのエンドポイントはそこに無い。ここで境界を吸収する。
			return misskeyApi(endpoint as never, (params ?? {}) as never, undefined, options?.signal) as never;
		},
		async fetch(path, options) {
			if ($i?.token == null || $i.token === '') throw new Error('Plugin fetch requires authentication');
			const url = new URL(path, window.location.origin);
			const rawPath = path.split('?')[0];
			if (!path.startsWith(pluginAPIPrefix) || url.origin !== window.location.origin ||
				!url.pathname.startsWith(pluginAPIPrefix) || /[%\\\s#]/.test(rawPath) || path.includes('#') ||
				rawPath.split('/').some(segment => segment === '.' || segment === '..') || url.searchParams.has('i')) {
				throw new Error(`Plugin fetch must stay inside ${pluginAPIPrefix}`);
			}
			const headers = new Headers(options?.headers);
			headers.set('Authorization', `Bearer ${$i.token}`);
			return window.fetch(`${url.pathname}${url.search}`, {
				...options,
				mode: 'same-origin',
				credentials: 'omit',
				redirect: 'error',
				headers,
			});
		},
		async openMedia(items, initialId) {
			if (items.length === 0) return;
			// URL生成より前に全件を検証し、型不一致やSVGを表示対象にしない。
			for (const item of items) {
				const mime = item.blob instanceof Blob ? item.blob.type.split(';')[0].trim().toLowerCase() : '';
				if (!['image', 'video', 'audio'].includes(item.type) || !mime.startsWith(`${item.type}/`) || !FILE_TYPE_BROWSERSAFE.includes(mime)) {
					throw new Error(`Plugin media ${item.id} has an invalid MIME type`);
				}
			}
			const objectUrls: string[] = [];
			let revoked = false;
			const revoke = (): void => {
				if (revoked) return;
				revoked = true;
				for (const url of objectUrls) URL.revokeObjectURL(url);
			};
			try {
				const contents = items.map<Content>(item => {
					const url = URL.createObjectURL(item.blob);
					objectUrls.push(url);
					return {
						id: item.id, type: item.type, url, thumbnailUrl: item.type === 'image' ? url : undefined,
						width: item.width, height: item.height, filename: item.filename, alt: item.alt,
						isSensitive: item.isSensitive === true,
					};
				});
				const os = await import('@/os.js');
				const { dispose } = await os.popupAsyncWithDialog(import('@/components/MkLightbox.vue').then(module => module.default), {
					defaultIndex: Math.max(0, contents.findIndex(content => content.id === initialId)),
					contents,
					initiallyRevealedContentIds: items.filter(item => item.initiallyRevealed === true).map(item => item.id),
				}, {
					closed: () => { dispose(); revoke(); },
				});
			} catch (err) {
				revoke();
				throw err;
			}
		},
		shouldHideSensitiveMedia(isSensitive) {
			return sensitive.shouldHideSensitiveMediaByDefault(isSensitive === true);
		},
		canRevealSensitiveMedia(isSensitive) {
			return sensitive.canRevealSensitiveMedia(isSensitive === true);
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
	if (plugins.length === 0) return;
	// 判定は設定とダイアログ (`@/os.js`) を読むので、モジュールの読み込みでは import しない。
	// 同期で呼べるよう、setup より前に 1 回だけ読む。読めなくても、プラグインの起動は止めない。
	const sensitive = await loadSensitiveMediaJudge();
	for (const p of plugins) {
		try {
			await p.setup(buildHost(p.name, sensitive));
		} catch (err) {
			console.error(`[plugin:${p.name}] setup failed`, err);
		}
	}
}

/** Test hook: clears registered slots. */
export function _resetSlotsForTest(): void {
	registry.clear();
}

/** Test hook: clears registered preview providers. */
export function _resetPreviewsForTest(): void {
	previewRegistry.clear();
}

/** Test hook: replaces how the sensitive media judge is loaded. Pass null to restore it. */
export function _setSensitiveMediaImporterForTest(importer: (() => Promise<SensitiveMediaJudge>) | null): void {
	sensitiveMediaJudgeImporter = importer ?? importSensitiveMediaJudge;
}

/** Test hook: clears registered post form paste transforms. */
export function _resetPasteTransformsForTest(): void {
	pasteTransformRegistry.clear();
}

/** Test hook: clears registered post form note options. */
export function _resetNoteOptionsForTest(): void {
	noteOptionRegistry.clear();
}
