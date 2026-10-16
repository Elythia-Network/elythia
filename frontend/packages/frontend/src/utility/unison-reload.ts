/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { TypedBroadcastChannel } from '@/utility/broadcast-channel.js';

export const reloadChannel = new TypedBroadcastChannel<string | null>('reload');

// BroadcastChannelを用いて、クライアントが一斉にreloadするようにします。
export function unisonReload(path?: string) {
	if (path !== undefined) {
		reloadChannel.postMessage(path);
		window.location.href = path;
	} else {
		reloadChannel.postMessage(null);
		window.location.reload();
	}
}
