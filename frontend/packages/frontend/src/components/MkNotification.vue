<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div :class="[$style.root, { [$style.contentVisibilityAuto]: contentVisibilityAuto }]">
	<div :class="$style.head">
		<MkAvatar v-if="['pollEnded', 'note'].includes(notification.type) && 'note' in notification" :class="$style.icon" :user="notification.note.user" link preview/>
		<MkAvatar v-else-if="['roleAssigned', 'achievementEarned', 'exportCompleted', 'login', 'createToken', 'scheduledNotePosted', 'scheduledNotePostFailed', 'emojiApplicationProcessed'].includes(notification.type)" :class="$style.icon" :user="$i" link preview/>
		<div v-else-if="notification.type === 'reaction:grouped' && notification.note.reactionAcceptance === 'likeOnly'" :class="[$style.icon, $style.icon_reactionGroupHeart]"><i class="ti ti-heart" style="line-height: 1;"></i></div>
		<div v-else-if="notification.type === 'reaction:grouped'" :class="[$style.icon, $style.icon_reactionGroup]"><i class="ti ti-plus" style="line-height: 1;"></i></div>
		<div v-else-if="notification.type === 'renote:grouped'" :class="[$style.icon, $style.icon_renoteGroup]"><i class="ti ti-repeat" style="line-height: 1;"></i></div>
		<!--
			**登録申請の受付通知には notifier がいない (#2987)。** 申請者はまだ
			アカウントを持っていないので、指せる利用者が存在しない。`'user' in
			notification` の受け皿より前に置かないと、下の汎用フォールバックに
			落ちてアイコンが空になる。
		-->
		<div v-else-if="isMkGoType(notification, 'signupApplicationReceived')" :class="[$style.icon, $style.icon_signupApplication]"><i class="ti ti-user-plus" style="line-height: 1;"></i></div>
		<MkAvatar v-else-if="'user' in notification" :class="$style.icon" :user="notification.user" link preview/>
		<img v-else-if="'icon' in notification && notification.icon != null" :class="[$style.icon, $style.icon_app]" :src="notification.icon" alt=""/>
		<div
			:class="[$style.subIcon, {
				[$style.t_follow]: notification.type === 'follow',
				[$style.t_followRequestAccepted]: notification.type === 'followRequestAccepted',
				[$style.t_receiveFollowRequest]: notification.type === 'receiveFollowRequest',
				[$style.t_renote]: notification.type === 'renote',
				[$style.t_reply]: notification.type === 'reply',
				[$style.t_mention]: notification.type === 'mention',
				[$style.t_quote]: notification.type === 'quote',
				[$style.t_pollEnded]: notification.type === 'pollEnded',
				[$style.t_scheduledNotePosted]: notification.type === 'scheduledNotePosted',
				[$style.t_scheduledNotePostFailed]: notification.type === 'scheduledNotePostFailed',
				[$style.t_achievementEarned]: notification.type === 'achievementEarned',
				[$style.t_exportCompleted]: notification.type === 'exportCompleted',
				[$style.t_login]: notification.type === 'login',
				[$style.t_createToken]: notification.type === 'createToken',
				[$style.t_chatRoomInvitationReceived]: notification.type === 'chatRoomInvitationReceived',
				[$style.t_roleAssigned]: notification.type === 'roleAssigned' && notification.role.iconUrl == null,
				[$style.t_emojiApplicationApproved]: isMkGoType(notification, 'emojiApplicationProcessed') && mkGoEmojiApplicationStatus(notification) === 'approved',
				[$style.t_emojiApplicationRejected]: isMkGoType(notification, 'emojiApplicationProcessed') && mkGoEmojiApplicationStatus(notification) !== 'approved',
				[$style.t_applicationReceived]: isMkGoType(notification, 'emojiApplicationReceived') || isMkGoType(notification, 'signupApplicationReceived'),
				[$style.t_abuseReport]: isMkGoType(notification, 'abuseReport') && !mkGoResolved(notification),
				[$style.t_abuseReportResolved]: isMkGoType(notification, 'abuseReport') && mkGoResolved(notification),
			}]"
		>
			<i v-if="notification.type === 'follow'" class="ti ti-plus"></i>
			<i v-else-if="notification.type === 'receiveFollowRequest'" class="ti ti-clock"></i>
			<i v-else-if="notification.type === 'followRequestAccepted'" class="ti ti-check"></i>
			<i v-else-if="notification.type === 'renote'" class="ti ti-repeat"></i>
			<i v-else-if="notification.type === 'reply'" class="ti ti-arrow-back-up"></i>
			<i v-else-if="notification.type === 'mention'" class="ti ti-at"></i>
			<i v-else-if="notification.type === 'quote'" class="ti ti-quote"></i>
			<i v-else-if="notification.type === 'pollEnded'" class="ti ti-chart-arrows"></i>
			<i v-else-if="notification.type === 'scheduledNotePosted'" class="ti ti-send"></i>
			<i v-else-if="notification.type === 'scheduledNotePostFailed'" class="ti ti-alert-triangle"></i>
			<i v-else-if="notification.type === 'achievementEarned'" class="ti ti-medal"></i>
			<i v-else-if="notification.type === 'exportCompleted'" class="ti ti-archive"></i>
			<i v-else-if="notification.type === 'login'" class="ti ti-login-2"></i>
			<i v-else-if="notification.type === 'createToken'" class="ti ti-key"></i>
			<i v-else-if="notification.type === 'chatRoomInvitationReceived'" class="ti ti-messages"></i>
			<!-- mk-go 固有 (#2868)。upstream は通報を通知欄に出さない。 -->
			<i v-else-if="isMkGoType(notification, 'emojiApplicationProcessed') && mkGoEmojiApplicationStatus(notification) === 'approved'" class="ti ti-mood-check"></i>
			<i v-else-if="isMkGoType(notification, 'emojiApplicationProcessed')" class="ti ti-mood-sad"></i>
			<i v-else-if="isMkGoType(notification, 'emojiApplicationReceived')" class="ti ti-mood-plus"></i>
			<i v-else-if="isMkGoType(notification, 'signupApplicationReceived')" class="ti ti-user-plus"></i>
			<i v-else-if="isMkGoType(notification, 'abuseReport') && mkGoResolved(notification)" class="ti ti-check"></i>
			<i v-else-if="isMkGoType(notification, 'abuseReport')" class="ti ti-exclamation-circle"></i>
			<template v-else-if="notification.type === 'roleAssigned'">
				<img v-if="notification.role.iconUrl" style="height: 1.3em; vertical-align: -22%;" :src="notification.role.iconUrl" alt=""/>
				<i v-else class="ti ti-badges"></i>
			</template>
			<MkReactionIcon
				v-else-if="notification.type === 'reaction'"
				:withTooltip="true"
				:reaction="notification.reaction.replace(/^:(\w+):$/, ':$1@.:')"
				:noStyle="true"
				style="width: 100%; height: 100% !important; object-fit: contain;"
			/>
		</div>
	</div>
	<div :class="$style.tail">
		<header :class="$style.header">
			<span v-if="notification.type === 'pollEnded'">{{ i18n.ts._notification.pollEnded }}</span>
			<span v-else-if="notification.type === 'scheduledNotePosted'">{{ i18n.ts._notification.scheduledNotePosted }}</span>
			<span v-else-if="notification.type === 'scheduledNotePostFailed'">{{ i18n.ts._notification.scheduledNotePostFailed }}</span>
			<span v-else-if="notification.type === 'note'">{{ i18n.ts._notification.newNote }}: <MkUserName :user="notification.note.user"/></span>
			<span v-else-if="notification.type === 'roleAssigned'">{{ i18n.ts._notification.roleAssigned }}</span>
			<span v-else-if="notification.type === 'chatRoomInvitationReceived'">{{ i18n.ts._notification.chatRoomInvitationReceived }}</span>
			<span v-else-if="notification.type === 'achievementEarned'">{{ i18n.ts._notification.achievementEarned }}</span>
			<span v-else-if="notification.type === 'login'">{{ i18n.ts._notification.login }}</span>
			<span v-else-if="notification.type === 'createToken'">{{ i18n.ts._notification.createToken }}</span>
			<span v-else-if="notification.type === 'test'">{{ i18n.ts._notification.testNotification }}</span>
			<span v-else-if="notification.type === 'exportCompleted'">{{ i18n.tsx._notification.exportOfXCompleted({ x: exportEntityName[notification.exportedEntity] }) }}</span>
			<MkA v-else-if="notification.type === 'follow' || notification.type === 'mention' || notification.type === 'reply' || notification.type === 'renote' || notification.type === 'quote' || notification.type === 'reaction' || notification.type === 'receiveFollowRequest' || notification.type === 'followRequestAccepted'" v-user-preview="notification.user.id" :class="$style.headerName" :to="userPage(notification.user)"><MkUserName :user="notification.user"/></MkA>
			<span v-else-if="notification.type === 'reaction:grouped' && notification.note.reactionAcceptance === 'likeOnly'">{{ i18n.tsx._notification.likedBySomeUsers({ n: getActualReactedUsersCount(notification) }) }}</span>
			<span v-else-if="notification.type === 'reaction:grouped'">{{ i18n.tsx._notification.reactedBySomeUsers({ n: getActualReactedUsersCount(notification) }) }}</span>
			<span v-else-if="notification.type === 'renote:grouped'">{{ i18n.tsx._notification.renotedBySomeUsers({ n: notification.users.length }) }}</span>
			<span v-else-if="notification.type === 'app'">{{ notification.header }}</span>
			<!--
				**「誰からの」を出す (#2868)。** アバターと名前は通報者のものなので、
				ヘッダが「新しい通報」だけだと通報された側と読み違えやすい。
			-->
			<span v-else-if="isMkGoType(notification, 'abuseReport') && mkGoNotifierName(notification) !== ''">
				{{ i18n.tsx._mkgoNotification.abuseReportFrom({ name: mkGoNotifierName(notification) }) }}
				<!--
					**対処済みは read 時に引き直した状態 (#2868)。** 通知は作成時点しか
					持たないので、これが無いと他のモデレーターが対処済みの通報に
					二重で当たる。
				-->
				<span v-if="mkGoResolved(notification)" :class="$style.abuseReportResolved">{{ i18n.ts._mkgoNotification.abuseReportResolved }}</span>
				<!--
					**未対応の件数 (#3200)。** 通報の通知はモデレーターごとに 1 件へまとめて
					いる (連打で通知欄が埋まらないように) ので、この通知が指す 1 件が
					対処済みでも後続が残っていることをここで出す。
				-->
				<span v-if="mkGoUnresolvedCount(notification) > 0" :class="$style.abuseReportPending">{{ i18n.tsx._mkgoNotification.abuseReportUnresolvedCount({ n: mkGoUnresolvedCount(notification) }) }}</span>
			</span>
			<!--
				申請が出された (#2987)。**処理済みかどうかは read 時に引き直した
				状態。** 通知は作成時点しか持たないので、これが無いと他の人が
				処理済みの申請に二重で当たる (abuseReport と同じ形)。
			-->
			<span v-else-if="isMkGoType(notification, 'emojiApplicationReceived')">
				{{ i18n.tsx._mkgoNotification.emojiApplicationReceived({ name: mkGoEmojiApplicationName(notification) }) }}
				<span v-if="mkGoEmojiApplicationStatus(notification) !== 'pending'" :class="$style.abuseReportResolved">{{ i18n.ts._mkgoNotification.applicationProcessed }}</span>
			</span>
			<span v-else-if="isMkGoType(notification, 'signupApplicationReceived')">
				{{ i18n.ts._mkgoNotification.signupApplicationReceived }}
				<span v-if="mkGoSignupApplicationStatus(notification) !== 'pending'" :class="$style.abuseReportResolved">{{ i18n.ts._mkgoNotification.applicationProcessed }}</span>
			</span>
			<span v-else-if="isMkGoType(notification, 'emojiApplicationProcessed')">
				<template v-if="mkGoEmojiApplicationStatus(notification) === 'approved'">
					{{ i18n.tsx._mkgoNotification.emojiApplicationApproved({ name: mkGoEmojiApplicationName(notification) }) }}
				</template>
				<template v-else>
					{{ i18n.tsx._mkgoNotification.emojiApplicationRejected({ name: mkGoEmojiApplicationName(notification) }) }}
					<!--
						**却下理由をここで出す。** 申請一覧を開かないと分からない形だと、
						通知は「駄目でした」としか伝えず、直して出し直す手がかりにならない。
					-->
					<div v-if="mkGoEmojiApplicationReason(notification) !== ''" :class="$style.emojiApplicationReason">
						{{ mkGoEmojiApplicationReason(notification) }}
					</div>
				</template>
			</span>
			<span v-else-if="isMkGoType(notification, 'abuseReport')">
				{{ i18n.ts._mkgoNotification.abuseReport }}
				<span v-if="mkGoUnresolvedCount(notification) > 0" :class="$style.abuseReportPending">{{ i18n.tsx._mkgoNotification.abuseReportUnresolvedCount({ n: mkGoUnresolvedCount(notification) }) }}</span>
			</span>
			<!--
				**未知の型の受け皿 (#2898)。** ここが無いと、mk-go 固有の通知や
				upstream が後から足した型がヘッダも本文も空で描画される
				(pollVote が実際にそうなっていた)。型名だけでも出しておけば、
				何が届いたのか分かる。
			-->
			<span v-else>{{ i18n.tsx._mkgoNotification.unknownType({ type: mkGoTypeName(notification) }) }}</span>
			<MkTime v-if="withTime" :time="notification.createdAt" :class="$style.headerTime"/>
		</header>
		<div>
			<MkA v-if="notification.type === 'reaction' || notification.type === 'reaction:grouped'" :class="$style.text" :to="notePage(notification.note)" :title="getNoteSummary(notification.note)">
				<i class="ti ti-quote" :class="$style.quote"></i>
				<Mfm :text="getNoteSummary(notification.note)" :plain="true" :nowrap="true" :author="notification.note.user"/>
				<i class="ti ti-quote" :class="$style.quote"></i>
			</MkA>
			<MkA v-else-if="notification.type === 'renote' || notification.type === 'renote:grouped'" :class="$style.text" :to="notePage(notification.note)" :title="getNoteSummary(notification.note.renote)">
				<i class="ti ti-quote" :class="$style.quote"></i>
				<Mfm :text="getNoteSummary(notification.note.renote)" :plain="true" :nowrap="true" :author="notification.note.renote?.user"/>
				<i class="ti ti-quote" :class="$style.quote"></i>
			</MkA>
			<MkA v-else-if="notification.type === 'reply'" :class="$style.text" :to="notePage(notification.note)" :title="getNoteSummary(notification.note)">
				<Mfm :text="getNoteSummary(notification.note)" :plain="true" :nowrap="true" :author="notification.note.user"/>
			</MkA>
			<MkA v-else-if="notification.type === 'mention'" :class="$style.text" :to="notePage(notification.note)" :title="getNoteSummary(notification.note)">
				<Mfm :text="getNoteSummary(notification.note)" :plain="true" :nowrap="true" :author="notification.note.user"/>
			</MkA>
			<MkA v-else-if="notification.type === 'quote'" :class="$style.text" :to="notePage(notification.note)" :title="getNoteSummary(notification.note)">
				<Mfm :text="getNoteSummary(notification.note)" :plain="true" :nowrap="true" :author="notification.note.user"/>
			</MkA>
			<MkA v-else-if="notification.type === 'note'" :class="$style.text" :to="notePage(notification.note)" :title="getNoteSummary(notification.note)">
				<Mfm :text="getNoteSummary(notification.note)" :plain="true" :nowrap="true" :author="notification.note.user"/>
			</MkA>
			<MkA v-else-if="notification.type === 'pollEnded'" :class="$style.text" :to="notePage(notification.note)" :title="getNoteSummary(notification.note)">
				<i class="ti ti-quote" :class="$style.quote"></i>
				<Mfm :text="getNoteSummary(notification.note)" :plain="true" :nowrap="true" :author="notification.note.user"/>
				<i class="ti ti-quote" :class="$style.quote"></i>
			</MkA>
			<MkA v-else-if="notification.type === 'scheduledNotePosted'" :class="$style.text" :to="notePage(notification.note)" :title="getNoteSummary(notification.note)">
				<i class="ti ti-quote" :class="$style.quote"></i>
				<Mfm :text="getNoteSummary(notification.note)" :plain="true" :nowrap="true" :author="notification.note.user"/>
				<i class="ti ti-quote" :class="$style.quote"></i>
			</MkA>
			<div v-else-if="notification.type === 'roleAssigned'" :class="$style.text">
				{{ notification.role.name }}
			</div>
			<div v-else-if="notification.type === 'chatRoomInvitationReceived'" :class="$style.text">
				{{ notification.invitation.room.name }}
			</div>
			<MkA v-else-if="notification.type === 'achievementEarned'" :class="$style.text" to="/my/achievements">
				{{ i18n.ts._achievements._types[`_${notification.achievement}`].title }}
			</MkA>
			<MkA v-else-if="notification.type === 'exportCompleted'" :class="$style.text" :to="`/my/drive/file/${notification.fileId}`">
				{{ i18n.ts.showFile }}
			</MkA>
			<MkA v-else-if="notification.type === 'createToken'" :class="$style.text" to="/settings/apps">
				<Mfm :text="i18n.tsx._notification.createTokenDescription({ text: i18n.ts.manageAccessTokens })"/>
			</MkA>
			<template v-else-if="notification.type === 'follow'">
				<span :class="$style.text" style="opacity: 0.6;">{{ i18n.ts.youGotNewFollower }}</span>
				<!--
					mk-go (#3185): フォローされた通知からそのままフォローを返す。フォロー申請の
					承認 / 拒否と同じく full のときだけ出す。**フォロー中なら文字だけ**
					(disableIfFollowing) — ボタンのままだと押してフォロー解除に進む。
					`user` は UserLite で `isFollowing` を持たないので、ボタンが users/show で
					補う (通知 1 件につき 1 回。CherryPick と同じ形)。
				-->
				<div v-if="full" :class="$style.followRequestCommands">
					<MkFollowButton :user="notification.user as Misskey.entities.UserDetailed" full disableIfFollowing/>
				</div>
			</template>
			<template v-else-if="notification.type === 'followRequestAccepted'">
				<div :class="$style.text" style="opacity: 0.6;">{{ i18n.ts.followRequestAccepted }}</div>
				<div v-if="notification.message" :class="$style.text" style="opacity: 0.6; font-style: oblique;">
					<i class="ti ti-quote" :class="$style.quote"></i>
					<Mfm :text="notification.message" :author="notification.user" :plain="true" :nowrap="true"/>
					<i class="ti ti-quote" :class="$style.quote"></i>
				</div>
			</template>
			<template v-else-if="notification.type === 'receiveFollowRequest'">
				<span :class="$style.text" style="opacity: 0.6;">{{ i18n.ts.receiveFollowRequest }}</span>
				<div v-if="full && !followRequestDone" :class="$style.followRequestCommands">
					<MkButton :class="$style.followRequestCommandButton" rounded primary @click="acceptFollowRequest()"><i class="ti ti-check"></i> {{ i18n.ts.accept }}</MkButton>
					<MkButton :class="$style.followRequestCommandButton" rounded danger @click="rejectFollowRequest()"><i class="ti ti-x"></i> {{ i18n.ts.reject }}</MkButton>
				</div>
			</template>
			<span v-else-if="notification.type === 'test'" :class="$style.text">{{ i18n.ts._notification.notificationWillBeDisplayedLikeThis }}</span>
			<span v-else-if="notification.type === 'app'" :class="$style.text">
				<Mfm :text="notification.body" :nowrap="false"/>
			</span>
			<!--
				通報 (#2868)。**本文は出さない。** 通報コメントは定型フォームの
				全文 (違反カテゴリ / 対象 / 該当 URL / 詳細) が入るので、通知欄に
				そのまま出すと長すぎて読めない。誰からの通報かだけ伝え、中身は
				ボタンから管理画面で見る。
			-->
			<!--
				審査画面への導線 (#2987)。**リンクが要点** — 通知欄で「申請が
				来た」と分かっても、対処するには結局どこから開くか探すことになる。
			-->
			<div v-else-if="isMkGoType(notification, 'emojiApplicationReceived') && full" :class="$style.abuseReportCommands">
				<!--
					**`/admin/*` ではなく `/custom-emojis-manager` へ送る。** 審査は
					`canManageCustomEmojis` でできるが、`/admin` 配下は
					`iAmModerator` gate を持つので、モデレーターではない絵文字
					管理者はそちらへ行くと not-found になる。
				-->
				<MkButton :class="$style.abuseReportCommandButton" type="routerLink" to="/custom-emojis-manager?tab=applications" rounded :primary="mkGoEmojiApplicationStatus(notification) === 'pending'"><i class="ti ti-mood-plus"></i> {{ i18n.ts._mkgoNotification.openApplication }}</MkButton>
			</div>
			<div v-else-if="isMkGoType(notification, 'signupApplicationReceived') && full" :class="$style.abuseReportCommands">
				<MkButton :class="$style.abuseReportCommandButton" type="routerLink" to="/admin/signup-applications" rounded :primary="mkGoSignupApplicationStatus(notification) === 'pending'"><i class="ti ti-user-plus"></i> {{ i18n.ts._mkgoNotification.openApplication }}</MkButton>
			</div>
			<div v-else-if="isMkGoType(notification, 'abuseReport') && full && mkGoExtra(notification, 'reportId') !== ''" :class="$style.abuseReportCommands">
				<MkButton :class="$style.abuseReportCommandButton" type="routerLink" :to="`/admin/abuses?reportId=${mkGoExtra(notification, 'reportId')}`" rounded :primary="!mkGoResolved(notification)"><i class="ti ti-exclamation-circle"></i> {{ i18n.ts._mkgoNotification.openModeration }}</MkButton>
				<!--
					まとめた通知から後続の通報へ行く口 (#3200)。この 1 件が対処済みなら
					こちらを主ボタンにする。
				-->
				<MkButton v-if="mkGoUnresolvedCount(notification) > 0" :class="$style.abuseReportCommandButton" type="routerLink" to="/admin/abuses" rounded :primary="mkGoResolved(notification)"><i class="ti ti-list"></i> {{ i18n.ts._mkgoNotification.openUnresolvedReports }}</MkButton>
			</div>

			<div v-if="notification.type === 'reaction:grouped'">
				<div v-for="reaction of notification.reactions" :key="reaction.user.id + reaction.reaction" :class="$style.reactionsItem">
					<MkAvatar :class="$style.reactionsItemAvatar" :user="reaction.user" link preview/>
					<div :class="$style.reactionsItemReaction">
						<MkReactionIcon
							:withTooltip="true"
							:reaction="reaction.reaction.replace(/^:(\w+):$/, ':$1@.:')"
							:noStyle="true"
							style="width: 100%; height: 100% !important; object-fit: contain;"
						/>
					</div>
				</div>
			</div>
			<div v-else-if="notification.type === 'renote:grouped'">
				<div v-for="user of notification.users" :key="user.id" :class="$style.reactionsItem">
					<MkAvatar :class="$style.reactionsItemAvatar" :user="user" link preview/>
				</div>
			</div>
		</div>
	</div>
</div>
</template>

<script lang="ts" setup>
import { ref } from 'vue';
import * as Misskey from 'misskey-js';
import MkReactionIcon from '@/components/MkReactionIcon.vue';
import MkButton from '@/components/MkButton.vue';
import MkFollowButton from '@/components/MkFollowButton.vue';
import { getNoteSummary } from '@/utility/get-note-summary.js';
import { notePage } from '@/filters/note.js';
import { userPage } from '@/filters/user.js';
import { i18n } from '@/i18n.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { ensureSignin } from '@/i.js';

const $i = ensureSignin();

const props = withDefaults(defineProps<{
	notification: Misskey.entities.Notification;
	withTime?: boolean;
	full?: boolean;
	contentVisibilityAuto?: boolean;
}>(), {
	withTime: false,
	full: false,
	contentVisibilityAuto: true,
});

type ExportCompletedNotification = Misskey.entities.Notification & { type: 'exportCompleted' };

const exportEntityName = {
	antenna: i18n.ts.antennas,
	blocking: i18n.ts.blockedUsers,
	clip: i18n.ts.clips,
	customEmoji: i18n.ts.customEmojis,
	favorite: i18n.ts.favorites,
	following: i18n.ts.following,
	muting: i18n.ts.mutedUsers,
	note: i18n.ts.notes,
	userList: i18n.ts.lists,
} as const satisfies Record<ExportCompletedNotification['exportedEntity'], string>;

const followRequestDone = ref(false);

const acceptFollowRequest = () => {
	if (!('user' in props.notification)) return;
	followRequestDone.value = true;
	misskeyApi('following/requests/accept', { userId: props.notification.user.id });
};

const rejectFollowRequest = () => {
	if (!('user' in props.notification)) return;
	followRequestDone.value = true;
	misskeyApi('following/requests/reject', { userId: props.notification.user.id });
};

function getActualReactedUsersCount(notification: Misskey.entities.Notification) {
	if (notification.type !== 'reaction:grouped') return 0;
	return new Set(notification.reactions.map((reaction) => reaction.user.id)).size;
}

/**
 * mk-go 固有の通知タイプを判定する (#2898)。
 *
 * misskey-js の autogen 型は upstream の通知タイプしか知らないので、
 * `notification.type === 'abuseReport'` と直接書くと型エラーになる。
 * 実行時は素の文字列比較で足りるため、ここで 1 箇所に閉じ込める
 * (mk-go 独自 endpoint を `as never` で呼ぶのと同じ理由)。
 */
function isMkGoType(notification: Misskey.entities.Notification, type: string): boolean {
	return (notification as { type: string }).type === type;
}

/**
 * Return a notification's raw type name.
 *
 * 全ての既知タイプを分岐で尽くした後の v-else では、autogen 型の narrowing で
 * `notification` が `never` になり `notification.type` が読めない。実行時には
 * 値が入っているので、型を外して読む。
 */
function mkGoTypeName(notification: Misskey.entities.Notification): string {
	return (notification as unknown as { type: string }).type;
}

/**
 * Read-time state of an emojiApplicationProcessed notification (#2934).
 *
 * backend が Extra["applicationId"] から引き直して `emojiApplication` として
 * 載せる。**通知そのものには結果を積んでいない** ので、ここに無ければ
 * 通知自体が届かない (backend が drop する)。
 */
function mkGoEmojiApplication(notification: Misskey.entities.Notification): Record<string, unknown> | null {
	const v = (notification as unknown as Record<string, unknown>).emojiApplication;
	return (v != null && typeof v === 'object') ? v as Record<string, unknown> : null;
}

function mkGoEmojiApplicationStatus(notification: Misskey.entities.Notification): string {
	return String(mkGoEmojiApplication(notification)?.status ?? '');
}

function mkGoEmojiApplicationName(notification: Misskey.entities.Notification): string {
	return String(mkGoEmojiApplication(notification)?.name ?? '');
}

function mkGoEmojiApplicationReason(notification: Misskey.entities.Notification): string {
	return String(mkGoEmojiApplication(notification)?.rejectReason ?? '');
}

/**
 * Whether the report behind an abuseReport notification is already resolved.
 *
 * サーバーが read 時に引き直して `resolved` を載せる (#2868)。autogen 型には
 * 無いので型を外して読む。
 */
function mkGoResolved(notification: Misskey.entities.Notification): boolean {
	return (notification as unknown as Record<string, unknown>).resolved === true;
}

/**
 * Number of unresolved reports across the instance (#3200).
 *
 * サーバーが read 時に数えて `unresolvedCount` を載せる。数えられなかったときは
 * 載らないので 0 を返し、件数表示ごと出さない。
 */
function mkGoUnresolvedCount(notification: Misskey.entities.Notification): number {
	const v = (notification as unknown as Record<string, unknown>).unresolvedCount;
	return typeof v === 'number' ? v : 0;
}

/**
 * Current status of the signup request a signupApplicationReceived
 * notification points at (#2987)。read 時に引き直した値。
 */
function mkGoSignupApplicationStatus(notification: Misskey.entities.Notification): string {
	const app = (notification as unknown as { signupApplication?: { status?: unknown } }).signupApplication;
	return typeof app?.status === 'string' ? app.status : '';
}

/**
 * Display name of a mk-go specific notification's notifier.
 *
 * 既知タイプを分岐で尽くした後では autogen 型の narrowing で `notification` が
 * `never` になり `user` が読めない。実行時には入っているので型を外して読む。
 * notifier を持たない型では空文字を返す。
 */
function mkGoNotifierName(notification: Misskey.entities.Notification): string {
	const u = (notification as unknown as { user?: { name?: string | null; username?: string } }).user;
	if (u == null) return '';
	return u.name ?? u.username ?? '';
}

/**
 * Read one field out of a mk-go specific notification's extra payload.
 *
 * mk-go は固有の通知の付随データを notification 直下へ spread する
 * (entity 側の Extra spread)。autogen 型には無いのでここで受ける。
 * 値が無い / 文字列でないときは空文字を返し、テンプレート側を壊さない。
 */
function mkGoExtra(notification: Misskey.entities.Notification, key: string): string {
	const v = (notification as unknown as Record<string, unknown>)[key];
	return typeof v === 'string' ? v : '';
}
</script>

<style lang="scss" module>
.root {
	position: relative;
	box-sizing: border-box;
	padding: 24px 32px;
	font-size: 0.9em;
	overflow-wrap: break-word;
	display: flex;
	contain: content;

	&.contentVisibilityAuto {
		content-visibility: auto;
		contain-intrinsic-size: 0 100px;
	}

	--eventFollow: #36aed2;
	--eventRenote: #36d298;
	--eventReply: #007aff;
	--eventReactionHeart: var(--MI_THEME-love);
	--eventReaction: #e99a0b;
	--eventAchievement: #cb9a11;
	--eventLogin: #007aff;
	--eventOther: #88a6b7;
}

.head {
	position: sticky;
	top: 0;
	flex-shrink: 0;
	width: 42px;
	height: 42px;
	margin-right: 8px;
}

.icon {
	display: block;
	width: 100%;
	height: 100%;
}

.icon_reactionGroup,
.icon_reactionGroupHeart,
.icon_renoteGroup {
	display: grid;
	align-items: center;
	justify-items: center;
	width: 80%;
	height: 80%;
	font-size: 15px;
	border-radius: 100%;
	color: #fff;
}

.icon_reactionGroup {
	background: var(--eventReaction);
}

/*
	登録申請の受付通知のアイコン (#2987)。notifier がいないのでアバターを
	出せない。`.icon_renoteGroup` と同じ寸法・字色の枠を使う。
*/
.icon_signupApplication {
	display: grid;
	align-items: center;
	justify-items: center;
	width: 80%;
	height: 80%;
	font-size: 15px;
	border-radius: 100%;
	color: #fff;
	background: var(--MI_THEME-accent);
}

.icon_reactionGroupHeart {
	background: var(--eventReactionHeart);
}

.icon_renoteGroup {
	background: var(--eventRenote);
}

.icon_app {
	border-radius: 6px;
}

.subIcon {
	position: absolute;
	z-index: 1;
	bottom: -2px;
	right: -2px;
	width: 20px;
	height: 20px;
	line-height: 20px;
	box-sizing: border-box;
	border-radius: 100%;
	background: var(--MI_THEME-panel);
	box-shadow: 0 0 0 3px var(--MI_THEME-panel);
	font-size: 11px;
	text-align: center;
	color: #fff;

	&:empty {
		display: none;
	}
}

.t_follow, .t_followRequestAccepted, .t_receiveFollowRequest {
	background: var(--eventFollow);
	pointer-events: none;
}

.t_renote {
	background: var(--eventRenote);
	pointer-events: none;
}

.t_quote {
	background: var(--eventRenote);
	pointer-events: none;
}

.t_reply {
	background: var(--eventReply);
	pointer-events: none;
}

.t_mention {
	background: var(--eventOther);
	pointer-events: none;
}

.t_pollEnded {
	background: var(--eventOther);
	pointer-events: none;
}

.t_scheduledNotePosted {
	background: var(--eventOther);
	pointer-events: none;
}

.t_scheduledNotePostFailed {
	background: var(--eventOther);
	pointer-events: none;
}

.t_achievementEarned {
	background: var(--eventAchievement);
	pointer-events: none;
}

.t_exportCompleted {
	background: var(--eventOther);
	pointer-events: none;
}

.t_roleAssigned {
	background: var(--eventOther);
	pointer-events: none;
}

/*
	申請の受付 (#2987)。**通報 (エラー色) とは分ける** — 通報は対処が要る
	出来事だが、申請は順番に見ればよいもの。同じ色にすると通知一覧で
	見分けが付かない。

	**padding を足さない。** `.subIcon` は `box-sizing: border-box` +
	`line-height: 20px` で中身を中央に置くので、padding を足すと内容領域だけ
	縮んでアイコンが下へずれる。#2868 が同じことをして本番で指摘され、
	外した経緯がある (他の t_* はどれも padding を持たない)。
*/
.t_applicationReceived {
	background: var(--MI_THEME-accent);
	pointer-events: none;
}

/*
	通報 (#2868)。**警告色 (--MI_THEME-warn) は使わない** — 実績の
	--eventAchievement (#cb9a11) とほぼ同じ黄色で、通知一覧で見分けが付かない
	(本番で「実績になっている」と指摘された)。対応が要るものなのでエラー色にする。
*/
.t_abuseReport {
	background: var(--MI_THEME-error);
	pointer-events: none;
}

/*
	絵文字の登録申請の結果 (#2934)。**背景を必ず指定する** — 既定は
	`background: var(--MI_THEME-panel); color: #fff` なので、light テーマ
	(panel が白系) では白地に白アイコンになって何も見えない (#2868 で同種の
	指摘が出ている)。承認と却下で色を分け、中身を読まなくても結果が分かるようにする。
*/
.t_emojiApplicationApproved {
	background: var(--MI_THEME-success);
	pointer-events: none;
}

.t_emojiApplicationRejected {
	background: var(--MI_THEME-error);
	pointer-events: none;
}

/* 対処済みは目立たせない (#2868)。未対応と並んだときに区別が付けばよい。 */
.t_abuseReportResolved {
	background: var(--eventOther);
	pointer-events: none;
}

.emojiApplicationReason {
	margin-top: 4px;
	padding: 6px 8px;
	border-radius: var(--MI-radius-xs);
	background: var(--MI_THEME-buttonBg);
	font-size: 0.9em;
	white-space: pre-wrap;
	word-break: break-word;
}

.abuseReportResolved {
	margin-left: 6px;
	padding: 1px 6px;
	border-radius: 4px;
	font-size: 0.8em;
	background: var(--MI_THEME-buttonBg);
	opacity: 0.8;
}

.abuseReportPending {
	margin-left: 6px;
	padding: 1px 6px;
	border-radius: 4px;
	font-size: 0.8em;
	color: var(--MI_THEME-error);
	background: color(from var(--MI_THEME-error) srgb r g b / 0.1);
}

.abuseReportCommands {
	display: flex;
	gap: 8px;
	margin-top: 6px;
}

.abuseReportCommandButton {
	font-size: 0.9em;
}

.t_login {
	background: var(--eventLogin);
	pointer-events: none;
}

.t_createToken {
	background: var(--eventOther);
	pointer-events: none;
}

.t_chatRoomInvitationReceived {
	background: var(--eventOther);
	pointer-events: none;
}

.tail {
	flex: 1;
	min-width: 0;
}

.header {
	display: flex;
	align-items: baseline;
	white-space: nowrap;
}

.headerName {
	text-overflow: ellipsis;
	white-space: nowrap;
	min-width: 0;
	overflow: hidden;
}

.headerTime {
	margin-left: auto;
	font-size: 0.9em;
}

.text {
	display: flex;
	width: 100%;
	overflow: clip;
}

.quote {
	vertical-align: super;
	font-size: 50%;
	opacity: 0.5;
}

.quote:first-child {
	margin-right: 4px;
	position: relative;

	&::before {
		position: absolute;
		transform: rotate(180deg);
	}
}

.quote:last-child {
	margin-left: 4px;
}

.followRequestCommands {
	display: flex;
	gap: 8px;
	max-width: 300px;
	margin-top: 8px;
}
.followRequestCommandButton {
	flex: 1;
}

.reactionsItem {
	display: inline-block;
	position: relative;
	width: 38px;
	height: 38px;
	margin-top: 8px;
	margin-right: 8px;
}

.reactionsItemAvatar {
	width: 100%;
	height: 100%;
}

.reactionsItemReaction {
	position: absolute;
	z-index: 1;
	bottom: -2px;
	right: -2px;
	width: 20px;
	height: 20px;
	box-sizing: border-box;
	border-radius: 100%;
	background: var(--MI_THEME-panel);
	box-shadow: 0 0 0 3px var(--MI_THEME-panel);
	font-size: 11px;
	text-align: center;
	color: #fff;
}

@container (max-width: 600px) {
	.root {
		padding: 16px;
		font-size: 0.9em;
	}
}

@container (max-width: 500px) {
	.root {
		padding: 12px;
		font-size: 0.85em;
	}
}
</style>
