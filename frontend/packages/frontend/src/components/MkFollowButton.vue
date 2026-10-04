<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<!--
	mk-go (#3185): disableIfFollowing のときは**フォローする以外の操作を出さない**。
	フォローされた通知から使うので、そこでフォロー解除 / 申請の取り消しができる必要は
	無い (ボタンのままだと「フォロー中」「申請中」を押すとそれぞれに進む)。
	**v-if / v-else を root に置く** — 1 つの root として扱われるので、親から渡る
	class / attrs はどれにも落ちる。
-->
<span v-if="disableIfFollowing && isFollowing" :class="$style.followingText"><i class="ti ti-check"></i> {{ i18n.ts.youFollowing }}</span>
<span v-else-if="disableIfFollowing && hasPendingFollowRequestFromYou" :class="$style.followingText"><i class="ti ti-hourglass-empty"></i> {{ isLocked ? i18n.ts.followRequestPending : i18n.ts.processing }}</span>
<!--
	状態が分かるまでは何も出さない。出すと相互フォローの行で「フォロー」が一瞬見えて
	「フォロー中」に切り替わり、users/show が失敗したとき (相手が凍結された等) は
	押せないボタンが残り続ける。
-->
<span v-else-if="disableIfFollowing && isFollowing == null"></span>
<button
	v-else
	class="_button"
	:class="[$style.root, { [$style.wait]: wait, [$style.active]: isFollowing || hasPendingFollowRequestFromYou, [$style.full]: full, [$style.large]: large }]"
	:disabled="wait"
	@click="onClick"
>
	<template v-if="!wait">
		<template v-if="hasPendingFollowRequestFromYou && isLocked">
			<span v-if="full" :class="$style.text">{{ i18n.ts.followRequestPending }}</span><i class="ti ti-hourglass-empty"></i>
		</template>
		<template v-else-if="hasPendingFollowRequestFromYou && !isLocked">
			<!-- つまりリモートフォローの場合。 -->
			<span v-if="full" :class="$style.text">{{ i18n.ts.processing }}</span><MkLoading :em="true" :colored="false"/>
		</template>
		<template v-else-if="isFollowing">
			<span v-if="full" :class="$style.text">{{ i18n.ts.youFollowing }}</span><i class="ti ti-minus"></i>
		</template>
		<template v-else-if="!isFollowing && isLocked">
			<span v-if="full" :class="$style.text">{{ i18n.ts.followRequest }}</span><i class="ti ti-plus"></i>
		</template>
		<template v-else-if="!isFollowing && !isLocked">
			<span v-if="full" :class="$style.text">{{ i18n.ts.follow }}</span><i class="ti ti-plus"></i>
		</template>
	</template>
	<template v-else>
		<span v-if="full" :class="$style.text">{{ i18n.ts.processing }}</span><MkLoading :em="true" :colored="false"/>
	</template>
</button>
</template>

<script lang="ts" setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue';
import * as Misskey from 'misskey-js';
import { host } from '@@/js/config.js';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { useStream } from '@/stream.js';
import { i18n } from '@/i18n.js';
import { claimAchievement } from '@/utility/achievements.js';
import { pleaseLogin } from '@/utility/please-login.js';
import { $i } from '@/i.js';
import { prefer } from '@/preferences.js';
import { haptic } from '@/utility/haptic.js';

const props = withDefaults(defineProps<{
	user: Misskey.entities.UserDetailed,
	full?: boolean,
	large?: boolean,
	/**
	 * Show plain text instead of a button when already following (#3185).
	 *
	 * mk-go 独自。CherryPick の同名オプションに相当するが、設定で切り替えずに
	 * 呼び出し側 (フォローされた通知) が固定で渡す。フォロー中・申請中は文字だけ出し、
	 * フォロー状態が分かるまではボタンを出さない — 分かる前に押すと、既にフォロー中の
	 * 相手へ following/create を送ってエラーになる。
	 */
	disableIfFollowing?: boolean,
}>(), {
	full: false,
	large: false,
	disableIfFollowing: false,
});

const emit = defineEmits<{
	(_: 'update:user', value: Misskey.entities.UserDetailed): void
}>();

const isFollowing = ref(props.user.isFollowing);
// mk-go (#3185): users/show で補ったときに鍵の有無も拾う。通知の `user` (UserLite) は
// `isLocked` を持たないので、拾わないとフォローした後に「フォロー申請中」ではなく
// 「処理中」のまま止まる (申請中かどうかの判定が鍵の有無に依存している)。
// **拾っていなければ props を読む (computed)。** ref に写すと、プロフィールの
// 引っ張って更新 (`user` の差し替え) に追従しなくなる。
const fetchedIsLocked = ref<boolean | null>(null);
const isLocked = computed(() => fetchedIsLocked.value ?? props.user.isLocked);
const hasPendingFollowRequestFromYou = ref(props.user.hasPendingFollowRequestFromYou);
const wait = ref(false);
const connection = useStream().useChannel('main');

if (props.user.isFollowing == null && $i) {
	misskeyApi('users/show', {
		userId: props.user.id,
	})
		.then(onFollowChange)
		// mk-go (#3185): 相手が凍結 / 削除されていると失敗する (フォローされた通知では
		// 普通に起きる)。握らないと未処理の reject になる。状態が分からないままなので、
		// disableIfFollowing のときはボタンを出さない。
		.catch(() => {});
}

function onFollowChange(user: Misskey.entities.UserDetailed) {
	if (user.id === props.user.id) {
		isFollowing.value = user.isFollowing;
		hasPendingFollowRequestFromYou.value = user.hasPendingFollowRequestFromYou;
		fetchedIsLocked.value = user.isLocked;
	}
}

async function onClick() {
	const isLoggedIn = await pleaseLogin({
		openOnRemote: {
			type: 'web',
			path: `/@${props.user.username}@${props.user.host ?? host}`,
		},
	});
	if (!isLoggedIn) return;

	wait.value = true;

	haptic();

	try {
		if (isFollowing.value) {
			const { canceled } = await os.confirm({
				type: 'warning',
				text: i18n.tsx.unfollowConfirm({ name: props.user.name || props.user.username }),
			});

			if (canceled) {
				wait.value = false;
				return;
			}

			await misskeyApi('following/delete', {
				userId: props.user.id,
			});
		} else if (hasPendingFollowRequestFromYou.value) {
			const { canceled } = await os.confirm({
				type: 'question',
				text: i18n.tsx.cancelFollowRequestConfirm({ name: props.user.name || props.user.username }),
			});

			if (canceled) {
				wait.value = false;
				return;
			}

			await misskeyApi('following/requests/cancel', {
				userId: props.user.id,
			});
			hasPendingFollowRequestFromYou.value = false;
		} else {
			if (prefer.s.alwaysConfirmFollow) {
				const { canceled } = await os.confirm({
					type: 'question',
					text: i18n.tsx.followConfirm({ name: props.user.name || props.user.username }),
				});

				if (canceled) {
					wait.value = false;
					return;
				}
			}

			await misskeyApi('following/create', {
				userId: props.user.id,
				withReplies: prefer.s.defaultFollowWithReplies,
			});
			emit('update:user', {
				...props.user,
				withReplies: prefer.s.defaultFollowWithReplies,
			});
			hasPendingFollowRequestFromYou.value = true;

			if ($i == null) {
				wait.value = false;
				return;
			}

			claimAchievement('following1');

			if ($i.followingCount >= 10) {
				claimAchievement('following10');
			}
			if ($i.followingCount >= 50) {
				claimAchievement('following50');
			}
			if ($i.followingCount >= 100) {
				claimAchievement('following100');
			}
			if ($i.followingCount >= 300) {
				claimAchievement('following300');
			}
		}
	} catch (err) {
		console.error(err);
	} finally {
		wait.value = false;
	}
}

onMounted(() => {
	connection.on('follow', onFollowChange);
	connection.on('unfollow', onFollowChange);
});

onBeforeUnmount(() => {
	connection.dispose();
});
</script>

<style lang="scss" module>
.followingText {
	font-size: 0.9em;
	opacity: 0.7;
}

.root {
	position: relative;
	display: inline-block;
	font-weight: bold;
	color: var(--MI_THEME-fgOnWhite);
	border: solid 1px var(--MI_THEME-accent);
	padding: 0;
	height: 31px;
	font-size: 16px;
	border-radius: 32px;
	background: #fff;

	&.full {
		padding: 0 8px 0 12px;
		font-size: 14px;
	}

	&.large {
		font-size: 16px;
		height: 38px;
		padding: 0 12px 0 16px;
	}

	&:not(.full) {
		width: 31px;
	}

	&:focus-visible {
		outline-offset: 2px;
	}

	&:hover {
		//background: mix($primary, #fff, 20);
	}

	&:active {
		//background: mix($primary, #fff, 40);
	}

	&.active {
		color: var(--MI_THEME-fgOnAccent);
		background: var(--MI_THEME-accent);

		&:hover {
			background: hsl(from var(--MI_THEME-accent) h s calc(l + 10));
			border-color: hsl(from var(--MI_THEME-accent) h s calc(l + 10));
		}

		&:active {
			background: hsl(from var(--MI_THEME-accent) h s calc(l - 10));
			border-color: hsl(from var(--MI_THEME-accent) h s calc(l - 10));
		}
	}

	&.wait {
		cursor: wait !important;
		opacity: 0.7;
	}
}

.text {
	margin-right: 6px;
}
</style>
