<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<PageWithHeader v-model:tab="tab" :actions="headerActions" :tabs="headerTabs" :swipable="true">
	<div :key="tab" class="_spacer" style="--MI_SPACER-w: 800px;">
		<MkPagination v-if="tab !== 'silent'" :paginator="paginator">
			<template #empty><MkResult type="empty" :text="i18n.ts.noFollowRequests"/></template>
			<template #default="{items}">
				<div class="mk-follow-requests _gaps">
					<div v-for="req in items" :key="req.id" class="user _panel">
						<MkAvatar class="avatar" :user="displayUser(req)" indicator link preview/>
						<div class="body">
							<div class="name">
								<MkA v-user-preview="displayUser(req).id" class="name" :to="userPage(displayUser(req))"><MkUserName :user="displayUser(req)"/></MkA>
								<p class="acct">@{{ acct(displayUser(req)) }}</p>
							</div>
							<div v-if="tab === 'list'" class="commands">
								<MkButton class="command" rounded primary @click="accept(displayUser(req))"><i class="ti ti-check"></i> {{ i18n.ts.accept }}</MkButton>
								<MkButton class="command" rounded danger @click="reject(displayUser(req))"><i class="ti ti-x"></i> {{ i18n.ts.reject }}</MkButton>
							</div>
							<div v-else class="commands">
								<MkButton class="command" rounded danger @click="cancel(displayUser(req))"><i class="ti ti-x"></i> {{ i18n.ts.cancel }}</MkButton>
							</div>
						</div>
					</div>
				</div>
			</template>
		</MkPagination>
		<div v-else class="_gaps">
			<MkInfo>{{ i18n.ts._followApproval.silentFollowsDescription }}</MkInfo>
			<MkResult v-if="silentLoaded && silentItems.length === 0" type="empty" :text="i18n.ts._followApproval.noSilentFollows"/>
			<div class="mk-follow-requests _gaps">
				<div v-for="item in silentItems" :key="item.id" class="user _panel">
					<MkAvatar class="avatar" :user="item.follower" indicator link preview/>
					<div class="body">
						<div class="name">
							<MkA v-user-preview="item.follower.id" class="name" :to="userPage(item.follower)"><MkUserName :user="item.follower"/></MkA>
							<p class="acct">@{{ acct(item.follower) }} · <MkTime :time="item.createdAt"/></p>
						</div>
						<div class="commands">
							<span v-if="notFollowerIds.has(item.follower.id)" :class="$style.notFollower">{{ i18n.ts._followApproval.notFollower }}</span>
							<MkButton v-else class="command" rounded danger @click="breakFollow(item.follower)"><i class="ti ti-user-x"></i> {{ i18n.ts.breakFollow }}</MkButton>
						</div>
					</div>
				</div>
			</div>
			<MkButton v-if="silentHasMore" :disabled="silentLoading" rounded style="margin: 0 auto;" @click="loadSilent(false)">{{ i18n.ts.loadMore }}</MkButton>
		</div>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import * as Misskey from 'misskey-js';
import { computed, markRaw, ref, watch } from 'vue';
import type * as Elythia from 'elythia-js';
import MkPagination from '@/components/MkPagination.vue';
import MkButton from '@/components/MkButton.vue';
import MkInfo from '@/components/MkInfo.vue';
import { userPage, acct } from '@/filters/user.js';
import * as os from '@/os.js';
import { i18n } from '@/i18n.js';
import { definePage } from '@/page.js';
import { $i } from '@/i.js';
import { Paginator } from '@/utility/paginator.js';
import { misskeyApi } from '@/utility/misskey-api.js';

const props = defineProps<{
	/** The initial tab (`list` / `sent` / `silent`). */
	tab?: string;
}>();

const tabs = ['list', 'sent', 'silent'];

// 鍵アカウントでなくても、期間の設定 (#3466) などで受け取った申請があれば
// 受け取った側を先に開く。
function defaultTab() {
	return $i?.isLocked || $i?.hasPendingReceivedFollowRequest ? 'list' : 'sent';
}

const tab = ref(defaultTab());

// `?tab=` は同じパスへの遷移でも変わるので watch で追う (custom-emojis-manager と同じ)。
// `?tab=` の無い URL へ移ったら既定のタブに戻す。
watch(() => props.tab, v => {
	tab.value = v != null && tabs.includes(v) ? v : defaultTab();
}, { immediate: true });

let paginator: Paginator<'following/requests/list' | 'following/requests/sent'>;

// 通知せずに受け入れたフォロー (Elythia 独自、#3466)。Paginator は本家の
// エンドポイントの型しか受けないので、ページ送りを自前で持つ。
const SILENT_PAGE_SIZE = 20;
const silentItems = ref<Elythia.SilentFollow[]>([]);
const silentLoaded = ref(false);
const silentLoading = ref(false);
const silentHasMore = ref(false);
// もうフォロワーではない人。記録 (silent_follow) は解除しても残る設計なので、
// 行は残して解除のボタンだけ外す。
const notFollowerIds = ref(new Set<string>());
// 読み込みの世代。タブを素早く切り替えたときに、古い読み込みの結果で上書き
// しないよう、また読み込み中でも最初からの読み直しを止めないようにする。
let silentGeneration = 0;

async function loadSilent(reset: boolean) {
	if (!reset && silentLoading.value) return;
	const generation = ++silentGeneration;
	silentLoading.value = true;
	try {
		const last = reset ? undefined : silentItems.value.at(-1);
		const page = await misskeyApi('following/silent/list', {
			limit: SILENT_PAGE_SIZE,
			...(last != null ? { untilId: last.id } : {}),
		});
		if (generation !== silentGeneration) return;
		silentItems.value = reset ? page : [...silentItems.value, ...page];
		silentHasMore.value = page.length === SILENT_PAGE_SIZE;
		silentLoaded.value = true;
	} finally {
		if (generation === silentGeneration) silentLoading.value = false;
	}
}

watch(tab, (newTab) => {
	if (newTab === 'list') {
		paginator = markRaw(new Paginator('following/requests/list', { limit: 10 }));
	} else if (newTab === 'sent') {
		paginator = markRaw(new Paginator('following/requests/sent', { limit: 10 }));
	} else {
		loadSilent(true);
	}
}, { immediate: true });

async function breakFollow(user: Misskey.entities.UserLite) {
	const { canceled } = await os.confirm({
		type: 'warning',
		text: i18n.ts.breakFollowConfirm,
	});
	if (canceled) return;

	try {
		await misskeyApi('following/invalidate', { userId: user.id });
	} catch (err) {
		// 既にフォロワーでなければ、エラーにせず状態だけ反映する。
		if ((err as { code?: string }).code !== 'NOT_FOLLOWING') {
			os.alert({ type: 'error', text: (err as { message?: string }).message });
			return;
		}
	}
	notFollowerIds.value = new Set([...notFollowerIds.value, user.id]);
}

function accept(user: Misskey.entities.UserLite) {
	os.apiWithDialog('following/requests/accept', { userId: user.id }).then(() => {
		paginator.reload();
	});
}

async function reject(user: Misskey.entities.UserLite) {
	const { canceled } = await os.confirm({
		type: 'question',
		text: i18n.tsx.rejectFollowRequestConfirm({ name: user.name || user.username }),
	});

	if (canceled) return;

	await os.apiWithDialog('following/requests/reject', { userId: user.id }).then(() => {
		paginator.reload();
	});
}

async function cancel(user: Misskey.entities.UserLite) {
	const { canceled } = await os.confirm({
		type: 'question',
		text: i18n.tsx.cancelFollowRequestConfirm({ name: user.name || user.username }),
	});

	if (canceled) return;

	await os.apiWithDialog('following/requests/cancel', { userId: user.id }).then(() => {
		paginator.reload();
	});
}

function displayUser(req: Misskey.entities.FollowingRequestsListResponse[number]) {
	return tab.value === 'list' ? req.follower : req.followee;
}

const headerActions = computed(() => []);

const headerTabs = computed(() => [
	{
		key: 'list',
		title: i18n.ts._followRequest.recieved,
		icon: 'ti ti-download',
	}, {
		key: 'sent',
		title: i18n.ts._followRequest.sent,
		icon: 'ti ti-upload',
	}, {
		key: 'silent',
		title: i18n.ts._followApproval.silentFollows,
		icon: 'ti ti-bell-off',
	},
]);

definePage(() => ({
	title: i18n.ts.followRequests,
	icon: 'ti ti-user-plus',
}));
</script>

<style lang="scss" scoped>
.mk-follow-requests {
	> .user {
		display: flex;
		padding: 16px;

		> .avatar {
			display: block;
			flex-shrink: 0;
			margin: 0 12px 0 0;
			width: 42px;
			height: 42px;
			border-radius: 8px;
		}

		> .body {
			display: flex;
			width: calc(100% - 54px);
			position: relative;
			flex-wrap: wrap;
			gap: 8px;

			> .name {
				flex: 1 1 50%;

				> .name,
				> .acct {
					display: block;
					white-space: nowrap;
					text-overflow: ellipsis;
					overflow: hidden;
					margin: 0;
				}

				> .name {
					line-height: 24px;
				}

				> .acct {
					line-height: 16px;
					opacity: 0.7;
				}
			}

			> .description {
				width: 55%;
				line-height: 42px;
				white-space: nowrap;
				overflow: hidden;
				text-overflow: ellipsis;
				opacity: 0.7;
				padding-right: 40px;
				padding-left: 8px;
				box-sizing: border-box;

				@media (max-width: 500px) {
					display: none;
				}
			}

			> .commands {
				display: flex;
				gap: 8px;
			}

			> .actions {
				position: absolute;
				top: 0;
				bottom: 0;
				right: 0;
				margin: auto 0;

				> button {
					padding: 12px;
				}
			}
		}
	}
}
</style>

<style lang="scss" module>
.notFollower {
	line-height: 42px;
	opacity: 0.7;
}
</style>
