/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { defineAsyncComponent } from 'vue';
import * as Misskey from 'misskey-js';
import { apiUrl } from '@@/js/config.js';
import type { UploaderFeatures } from '@/composables/use-uploader.js';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { useStream } from '@/stream.js';
import { i18n } from '@/i18n.js';
import { prefer } from '@/preferences.js';
import { $i } from '@/i.js';
import { instance } from '@/instance.js';
import { globalEvents } from '@/events.js';
import { getProxiedImageUrl } from '@/utility/media-proxy.js';
import { genId } from '@/utility/id.js';

type UploadReturnType = {
	filePromise: Promise<Misskey.entities.DriveFile>;
	abort: () => void;
};

export class UploadAbortedError extends Error {
	constructor() {
		super('Upload aborted');
	}
}

type UploadOptions = {
	name?: string;
	folderId?: string | null;
	isSensitive?: boolean;
	caption?: string | null;
	onProgress?: (ctx: { total: number; loaded: number; }) => void;
};

type ChunkedUploadCapability = {
	chunkSize: number;
};

/**
 * mk-go が `/api/meta` で告知する分割アップロードの能力を読む。
 *
 * 純正 Misskey や、オブジェクトストレージ未設定 / 機能無効の mk-go では field
 * ごと存在しないので null が返り、従来の単発アップロードに倒れる。
 *
 * チャンクサイズはサーバーの告知に従い、こちら側にハードコードしない。S3 互換
 * サービスごとに最小パートサイズや「最終パート以外は同一サイズ」といった制約が
 * 異なり、サーバー側でしか決められないため。
 */
function getChunkedUploadCapability(): ChunkedUploadCapability | null {
	const cap = (instance as typeof instance & { chunkedUpload?: { chunkSize?: unknown } }).chunkedUpload;
	if (cap == null) return null;
	const chunkSize = cap.chunkSize;
	if (typeof chunkSize !== 'number' || !Number.isFinite(chunkSize) || chunkSize <= 0) return null;
	return { chunkSize };
}

/**
 * アップロード失敗時のダイアログ表示。単発経路と分割経路で共通。
 *
 * 経路によって同じ制約が別のメッセージになると利用者が混乱するので、error id /
 * code の分岐はここに一本化する。
 */
function showUploadError(status: number, rawResponse: string | null): void {
	if (status === 413) {
		os.alert({
			type: 'error',
			title: i18n.ts.failedToUpload,
			text: i18n.ts.cannotUploadBecauseExceedsFileSizeLimit,
		});
		return;
	}

	if (!rawResponse) {
		os.alert({
			type: 'error',
			title: 'Failed to upload',
			text: `${JSON.stringify(rawResponse)}`,
		});
		return;
	}

	let res: any;
	try {
		res = JSON.parse(rawResponse);
	} catch {
		os.alert({
			type: 'error',
			title: 'Failed to upload',
			text: rawResponse,
		});
		return;
	}

	if (res.error?.id === 'bec5bd69-fba3-43c9-b4fb-2894b66ad5d2') {
		os.alert({
			type: 'error',
			title: i18n.ts.failedToUpload,
			text: i18n.ts.cannotUploadBecauseInappropriate,
		});
	} else if (res.error?.id === 'd08dbc37-a6a9-463a-8c47-96c32ab5f064') {
		os.alert({
			type: 'error',
			title: i18n.ts.failedToUpload,
			text: i18n.ts.cannotUploadBecauseNoFreeSpace,
		});
	} else if (res.error?.id === '4becd248-7f2c-48c4-a9f0-75edc4f9a1ea') {
		os.alert({
			type: 'error',
			title: i18n.ts.failedToUpload,
			text: i18n.ts.cannotUploadBecauseUnallowedFileType,
		});
	// 以下は mk-go の分割アップロード固有 (#2313)。純正 Misskey では発生しない。
	} else if (res.error?.code === 'CHUNKED_UPLOAD_NOT_ALLOWED') {
		os.alert({
			type: 'error',
			title: i18n.ts.failedToUpload,
			text: i18n.ts._chunkedUpload.notAllowed,
		});
	} else if (res.error?.code === 'TOO_MANY_UPLOAD_SESSIONS' || res.error?.code === 'PENDING_UPLOAD_LIMIT_EXCEEDED') {
		os.alert({
			type: 'error',
			title: i18n.ts.failedToUpload,
			text: i18n.ts._chunkedUpload.tooManySessions,
		});
	} else if (res.error?.code === 'NO_SUCH_UPLOAD_SESSION') {
		os.alert({
			type: 'error',
			title: i18n.ts.failedToUpload,
			text: i18n.ts._chunkedUpload.sessionExpired,
		});
	} else {
		os.alert({
			type: 'error',
			title: i18n.ts.failedToUpload,
			text: `${res.error?.message}\n${res.error?.code}\n${res.error?.id}`,
		});
	}
}

export function uploadFile(file: File | Blob, options: UploadOptions = {}): UploadReturnType {
	const chunked = getChunkedUploadCapability();
	// 閾値もサーバー告知の chunkSize に従う。1 チャンクに収まるなら分割する意味が
	// 無いので、従来経路のほうがラウンドトリップが少ない。
	if (chunked != null && file.size > chunked.chunkSize) {
		return uploadFileChunked(file, options, chunked);
	}
	return uploadFileSingle(file, options);
}

function uploadFileSingle(file: File | Blob, options: UploadOptions = {}): UploadReturnType {
	const xhr = new XMLHttpRequest();
	const abortController = new AbortController();
	const { signal } = abortController;

	const filePromise = new Promise<Misskey.entities.DriveFile>((resolve, reject) => {
		if ($i == null) return reject();

		// こっち側で検出するMIME typeとサーバーで検出するMIME typeは異なる場合があるため、こっち側ではやらないことにする
		// https://github.com/misskey-dev/misskey/issues/16091
		//const allowedMimeTypes = $i.policies.uploadableFileTypes;
		//const isAllowedMimeType = allowedMimeTypes.some(mimeType => {
		//	if (mimeType === '*' || mimeType === '*/*') return true;
		//	if (mimeType.endsWith('/*')) return file.type.startsWith(mimeType.slice(0, -1));
		//	return file.type === mimeType;
		//});
		//if (!isAllowedMimeType) {
		//	os.alert({
		//		type: 'error',
		//		title: i18n.ts.failedToUpload,
		//		text: i18n.ts.cannotUploadBecauseUnallowedFileType,
		//	});
		//	return reject();
		//}

		if ((file.size > instance.maxFileSize) || (file.size > ($i.policies.maxFileSizeMb * 1024 * 1024))) {
			os.alert({
				type: 'error',
				title: i18n.ts.failedToUpload,
				text: i18n.ts.cannotUploadBecauseExceedsFileSizeLimit,
			});
			return reject();
		}

		signal.addEventListener('abort', () => {
			reject(new UploadAbortedError());
		}, { once: true });

		xhr.open('POST', apiUrl + '/drive/files/create', true);
		xhr.onload = ((ev: ProgressEvent<XMLHttpRequest>) => {
			if (xhr.status !== 200 || ev.target == null || ev.target.response == null) {
				showUploadError(xhr.status, ev.target?.response ?? null);
				reject();
				return;
			}

			const driveFile = JSON.parse(ev.target.response);
			globalEvents.emit('driveFileCreated', driveFile);
			resolve(driveFile);
		}) as (ev: ProgressEvent<EventTarget>) => void;

		// **トランスポート層の失敗も settle させる (#2959)。** `onload` しか
		// 無いと、接続断・サーバー再起動・回線切替で **promise が永久に
		// pending** になる。呼び出し側が「アップロード中」で UI を塞ぐ作りだと
		// (投稿フォームの uploader も、絵文字申請のフォームもそう)、リロード
		// 以外に復旧手段が無くなり入力中の内容ごと失う。**chunked 経路は
		// 既に `xhr.onerror` を持っており、単発経路だけ落ちていた。**
		xhr.onerror = () => {
			// 他の reject 経路と同じく、ここでダイアログを出しておく。
			// 呼び出し側で二重に出さないための約束 (中断だけが例外)。
			os.alert({
				type: 'error',
				title: i18n.ts.failedToUpload,
				text: i18n.ts.somethingHappened,
			});
			reject();
		};
		xhr.ontimeout = xhr.onerror;

		if (options.onProgress) {
			xhr.upload.onprogress = ev => {
				if (ev.lengthComputable && options.onProgress != null) {
					options.onProgress({
						total: ev.total,
						loaded: ev.loaded,
					});
				}
			};
		}

		const formData = new FormData();
		formData.append('i', $i.token);
		formData.append('force', 'true');
		formData.append('file', file);
		formData.append('name', options.name ?? (file instanceof File ? file.name : 'untitled'));
		formData.append('isSensitive', options.isSensitive ? 'true' : 'false');
		if (options.caption != null) formData.append('comment', options.caption);
		if (options.folderId) formData.append('folderId', options.folderId);

		xhr.send(formData);
	});

	const abort = () => {
		xhr.abort();
		abortController.abort();
	};

	return { filePromise, abort };
}

/**
 * mk-go の分割アップロード (#2313)。ファイルを固定サイズのチャンクに割り、
 * start / append / finish の 3 段階で送る。
 *
 * リバースプロキシのボディサイズ上限 (Cloudflare は 100MB) を超えるファイルを
 * 送るための経路。1 リクエストあたりの本文はサーバーが告知した chunkSize に
 * 収まるので、上限を上げられない環境でも通る。
 */
function uploadFileChunked(file: File | Blob, options: UploadOptions, cap: ChunkedUploadCapability): UploadReturnType {
	const abortController = new AbortController();
	const { signal } = abortController;
	// 進行中の append。abort() から中断できるよう保持する。
	let currentXhr: XMLHttpRequest | null = null;
	let uploadId: string | null = null;

	// **ダイアログを出したかを持つ (#2959)。** 単発経路は reject の前に必ず
	// 1 枚出す契約で、呼び出し側はそれに乗って空 catch にしている。chunked 側は
	// `appendChunk` の `onerror` と `post` の fetch 失敗で何も出さずに reject
	// しており、**大きい画像で回線が切れると無言で終わっていた**。同じ契約に
	// 揃える。
	let reported = false;

	const post = async (endpoint: string, body: Record<string, unknown>): Promise<any> => {
		const res = await window.fetch(`${apiUrl}/${endpoint}`, {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ ...body, i: $i!.token }),
		});
		const text = await res.text();
		if (!res.ok) {
			showUploadError(res.status, text || null);
			reported = true;
			throw new Error(`${endpoint} failed: ${res.status}`);
		}
		return text ? JSON.parse(text) : null;
	};

	const filePromise = new Promise<Misskey.entities.DriveFile>((resolve, reject) => {
		if ($i == null) return reject();

		if ((file.size > instance.maxFileSize) || (file.size > ($i.policies.maxFileSizeMb * 1024 * 1024))) {
			os.alert({
				type: 'error',
				title: i18n.ts.failedToUpload,
				text: i18n.ts.cannotUploadBecauseExceedsFileSizeLimit,
			});
			return reject();
		}

		signal.addEventListener('abort', () => {
			reject(new UploadAbortedError());
		}, { once: true });

		void (async () => {
			try {
				const name = options.name ?? (file instanceof File ? file.name : 'untitled');
				const session = await post('drive/files/create-chunked/start', {
					name,
					size: file.size,
					force: true,
					isSensitive: options.isSensitive ?? false,
					comment: options.caption ?? null,
					folderId: options.folderId ?? null,
				});
				if (signal.aborted) return;
				uploadId = session.uploadId;

				// サーバーが返した chunkSize に従う (start と告知の間に admin が
				// 設定を変えた場合、セッションに固定された値はこちら)。
				const chunkSize: number = session.chunkSize ?? cap.chunkSize;
				const totalChunks: number = session.totalChunks;

				for (let index = 0; index < totalChunks; index++) {
					if (signal.aborted) return;
					const start = index * chunkSize;
					const blob = file.slice(start, Math.min(start + chunkSize, file.size));
					await appendChunk(uploadId!, index, blob, start);
				}
				if (signal.aborted) return;

				const driveFile = await post('drive/files/create-chunked/finish', { uploadId });
				uploadId = null; // finish 済みなので abort で破棄しない
				globalEvents.emit('driveFileCreated', driveFile);
				resolve(driveFile);
			} catch (err) {
				if (signal.aborted) return;
				// 誰もダイアログを出していなければここで出す (transport の
				// 失敗がこれに当たる)。中断は `signal.aborted` で抜けている。
				if (!reported) {
					os.alert({
						type: 'error',
						title: i18n.ts.failedToUpload,
						text: i18n.ts.somethingHappened,
					});
				}
				reject(err);
			}
		})();
	});

	// チャンク 1 つを送る。進捗は「確定済みバイト数 + 送信中チャンクの loaded」で
	// 合算し、単発アップロードと同じ体験にする。
	function appendChunk(id: string, index: number, blob: Blob, uploadedBefore: number): Promise<void> {
		return new Promise<void>((resolve, reject) => {
			const xhr = new XMLHttpRequest();
			currentXhr = xhr;
			xhr.open('POST', `${apiUrl}/drive/files/create-chunked/append`, true);
			xhr.onload = () => {
				currentXhr = null;
				if (xhr.status !== 200) {
					showUploadError(xhr.status, xhr.response ?? null);
					reported = true;
					reject(new Error(`append ${index} failed: ${xhr.status}`));
					return;
				}
				options.onProgress?.({ total: file.size, loaded: uploadedBefore + blob.size });
				resolve();
			};
			xhr.onerror = () => {
				currentXhr = null;
				reject(new Error(`append ${index} failed`));
			};
			xhr.onabort = () => {
				currentXhr = null;
				reject(new UploadAbortedError());
			};
			if (options.onProgress) {
				xhr.upload.onprogress = ev => {
					if (ev.lengthComputable) {
						options.onProgress?.({ total: file.size, loaded: uploadedBefore + ev.loaded });
					}
				};
			}

			const formData = new FormData();
			formData.append('i', $i!.token);
			formData.append('uploadId', id);
			formData.append('index', String(index));
			formData.append('chunk', blob);
			xhr.send(formData);
		});
	}

	const abort = () => {
		currentXhr?.abort();
		abortController.abort();
		// サーバー側にセッションを残さない。放置すると未完了のマルチパート
		// アップロードが期限切れ GC まで残り、オブジェクトストレージの課金対象に
		// なる。best-effort なので失敗は握り潰す。
		if (uploadId != null) {
			const id = uploadId;
			uploadId = null;
			void window.fetch(`${apiUrl}/drive/files/create-chunked/abort`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ i: $i?.token, uploadId: id }),
				keepalive: true,
			}).catch(() => {});
		}
	};

	return { filePromise, abort };
}

export function chooseFileFromPcAndUpload(
	options: {
		multiple?: boolean;
		features?: UploaderFeatures;
		folderId?: string | null;
	} = {},
): Promise<Misskey.entities.DriveFile[]> {
	return new Promise((res, rej) => {
		os.chooseFileFromPc({ multiple: options.multiple }).then(files => {
			if (files.length === 0) return;
			os.launchUploader(files, {
				folderId: options.folderId,
				features: options.features,
			}).then(driveFiles => {
				res(driveFiles);
			});
		});
	});
}

export function chooseDriveFile(options: {
	multiple?: boolean;
} = {}): Promise<Misskey.entities.DriveFile[]> {
	return new Promise((resolve, rej) => {
		let dispose: () => void;
		os.popupAsyncWithDialog(import('@/components/MkDriveFileSelectDialog.vue').then(x => x.default), {
			multiple: options.multiple ?? false,
		}, {
			done: files => {
				if (files) {
					resolve(files);
				}
			},
			closed: () => dispose(),
		}).then((d) => dispose = d.dispose, rej);
	});
}

export function chooseFileFromUrl(): Promise<Misskey.entities.DriveFile> {
	return new Promise((res, rej) => {
		os.inputText({
			title: i18n.ts.uploadFromUrl,
			type: 'url',
			placeholder: i18n.ts.uploadFromUrlDescription,
		}).then(({ canceled, result: url }) => {
			if (canceled || url == null) return;

			const marker = genId();

			// TODO: no websocketモード対応
			const connection = useStream().useChannel('main');
			connection.on('urlUploadFinished', urlResponse => {
				if (urlResponse.marker === marker) {
					res(urlResponse.file);
					connection.dispose();
				}
			});

			misskeyApi('drive/files/upload-from-url', {
				url: url,
				folderId: prefer.s.uploadFolder,
				marker,
			});

			os.alert({
				title: i18n.ts.uploadFromUrlRequested,
				text: i18n.ts.uploadFromUrlMayTakeTime,
			});
		});
	});
}

function select(anchorElement: HTMLElement | EventTarget | null, label: string | null, multiple: boolean, features?: UploaderFeatures): Promise<Misskey.entities.DriveFile[]> {
	return new Promise((res, rej) => {
		os.popupMenu([label ? {
			text: label,
			type: 'label',
		} : null, {
			text: i18n.ts.upload,
			icon: 'ti ti-upload',
			action: () => chooseFileFromPcAndUpload({ multiple, features }).then(files => res(files)),
		}, {
			text: i18n.ts.fromDrive,
			icon: 'ti ti-cloud',
			action: () => chooseDriveFile({ multiple }).then(files => res(files)),
		}, {
			text: i18n.ts.fromUrl,
			icon: 'ti ti-link',
			action: () => chooseFileFromUrl().then(file => res([file])),
		}], anchorElement);
	});
}

type SelectFileOptions<M extends boolean> = {
	anchorElement: HTMLElement | EventTarget | null;
	multiple: M;
	label?: string | null;
	features?: UploaderFeatures;
};

export async function selectFile<
	M extends boolean,
	MR extends M extends true ? Misskey.entities.DriveFile[] : Misskey.entities.DriveFile,
>(opts: SelectFileOptions<M>): Promise<MR> {
	const files = await select(opts.anchorElement, opts.label ?? null, opts.multiple ?? false, opts.features);
	return opts.multiple ? (files as MR) : (files[0]! as MR);
}

export async function createCroppedImageDriveFileFromImageDriveFile(imageDriveFile: Misskey.entities.DriveFile, options: {
	aspectRatio: number | null;
}): Promise<Misskey.entities.DriveFile> {
	return new Promise((resolve, reject) => {
		const imgUrl = getProxiedImageUrl(imageDriveFile.url, undefined, true);
		const image = new Image();
		image.src = imgUrl;
		image.onload = () => {
			const canvas = window.document.createElement('canvas');
			const ctx = canvas.getContext('2d')!;
			canvas.width = image.width;
			canvas.height = image.height;
			ctx.drawImage(image, 0, 0);
			canvas.toBlob(blob => {
				if (blob == null) {
					reject();
					return;
				}

				os.cropImageFile(blob, {
					aspectRatio: options.aspectRatio,
				}).then(croppedImageFile => {
					const { filePromise } = uploadFile(croppedImageFile, {
						name: imageDriveFile.name,
						folderId: imageDriveFile.folderId,
					});

					filePromise.then(driveFile => {
						resolve(driveFile);
					});
				});
			});
		};
	});
}

export async function selectDriveFolder(initialFolder: Misskey.entities.DriveFolder['id'] | null): Promise<{
	canceled: false;
	folders: (Misskey.entities.DriveFolder | null)[];
} | {
	canceled: true;
	folders: undefined;
}> {
	return new Promise((resolve, reject) => {
		let dispose: () => void;
		os.popupAsyncWithDialog(import('@/components/MkDriveFolderSelectDialog.vue').then(x => x.default), {
			initialFolder,
		}, {
			done: folders => {
				resolve(folders == null ? {
					canceled: true,
					folders: undefined,
				} : {
					canceled: false,
					folders,
				});
			},
			closed: () => dispose(),
		}).then(d => dispose = d.dispose, reject);
	});
}
