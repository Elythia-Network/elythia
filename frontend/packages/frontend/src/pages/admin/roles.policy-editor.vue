<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
	<div class="_gaps_s">
		<XFolder v-if="matchQuery([i18n.ts._role._options.rateLimitFactor, 'rateLimitFactor'])" v-model:policyMeta="policyMetaModel.rateLimitFactor" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.rateLimitFactor }}</template>
			<template #valueText>{{ Math.floor(valuesModel.rateLimitFactor * 100) }}%</template>
			<template #default="{ disabled }">
				<MkRange v-model="valuesModel.rateLimitFactor" :disabled="disabled" :min="0.3" :max="3" :step="0.1" :textConverter="(v) => `${Math.round(v * 100)}%`">
					<template #caption>{{ i18n.ts._role._options.descriptionOfRateLimitFactor }}</template>
				</MkRange>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.gtlAvailable, 'gtlAvailable'])" v-model:policyMeta="policyMetaModel.gtlAvailable" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.gtlAvailable }}</template>
			<template #valueText>{{ valuesModel.gtlAvailable ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.gtlAvailable" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.ltlAvailable, 'ltlAvailable'])" v-model:policyMeta="policyMetaModel.ltlAvailable" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.ltlAvailable }}</template>
			<template #valueText>{{ valuesModel.ltlAvailable ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.ltlAvailable" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canPublicNote, 'canPublicNote'])" v-model:policyMeta="policyMetaModel.canPublicNote" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canPublicNote }}</template>
			<template #valueText>{{ valuesModel.canPublicNote ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canPublicNote" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.chatAvailability, 'chatAvailability'])" v-model:policyMeta="policyMetaModel.chatAvailability" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.chatAvailability }}</template>
			<template #valueText>{{ valuesModel.chatAvailability === 'available' ? i18n.ts.yes : valuesModel.chatAvailability === 'readonly' ? i18n.ts.readonly : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSelect
					v-model="valuesModel.chatAvailability"
					:disabled="disabled"
					:items="[
						{ label: i18n.ts.enabled, value: 'available' },
						{ label: i18n.ts.readonly, value: 'readonly' },
						{ label: i18n.ts.disabled, value: 'unavailable' },
					]"
				>
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSelect>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.mentionMax, 'mentionLimit'])" v-model:policyMeta="policyMetaModel.mentionLimit" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.mentionMax }}</template>
			<template #valueText>{{ valuesModel.mentionLimit }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.mentionLimit" type="number" :disabled="disabled">
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canInvite, 'canInvite'])" v-model:policyMeta="policyMetaModel.canInvite" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canInvite }}</template>
			<template #valueText>{{ valuesModel.canInvite ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canInvite" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.inviteLimit, 'inviteLimit'])" v-model:policyMeta="policyMetaModel.inviteLimit" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.inviteLimit }}</template>
			<template #valueText>{{ valuesModel.inviteLimit }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.inviteLimit" type="number" :disabled="disabled">
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.inviteLimitCycle, 'inviteLimitCycle'])" v-model:policyMeta="policyMetaModel.inviteLimitCycle" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.inviteLimitCycle }}</template>
			<template #valueText>{{ valuesModel.inviteLimitCycle + i18n.ts._time.minute }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.inviteLimitCycle" type="number" :disabled="disabled">
					<template #suffix>{{ i18n.ts._time.minute }}</template>
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.inviteExpirationTime, 'inviteExpirationTime'])" v-model:policyMeta="policyMetaModel.inviteExpirationTime" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.inviteExpirationTime }}</template>
			<template #valueText>{{ valuesModel.inviteExpirationTime + i18n.ts._time.minute }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.inviteExpirationTime" type="number" :disabled="disabled">
					<template #suffix>{{ i18n.ts._time.minute }}</template>
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canManageAvatarDecorations, 'canManageAvatarDecorations'])" v-model:policyMeta="policyMetaModel.canManageAvatarDecorations" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canManageAvatarDecorations }}</template>
			<template #valueText>{{ valuesModel.canManageAvatarDecorations ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canManageAvatarDecorations" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canManageCustomEmojis, 'canManageCustomEmojis'])" v-model:policyMeta="policyMetaModel.canManageCustomEmojis" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canManageCustomEmojis }}</template>
			<template #valueText>{{ valuesModel.canManageCustomEmojis ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canManageCustomEmojis" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canSearchNotes, 'canSearchNotes'])" v-model:policyMeta="policyMetaModel.canSearchNotes" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canSearchNotes }}</template>
			<template #valueText>{{ valuesModel.canSearchNotes ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canSearchNotes" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canSearchUsers, 'canSearchUsers'])" v-model:policyMeta="policyMetaModel.canSearchUsers" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canSearchUsers }}</template>
			<template #valueText>{{ valuesModel.canSearchUsers ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canSearchUsers" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canUseTranslator, 'canUseTranslator'])" v-model:policyMeta="policyMetaModel.canUseTranslator" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canUseTranslator }}</template>
			<template #valueText>{{ valuesModel.canUseTranslator ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canUseTranslator" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canCreateChannel, 'canCreateChannel'])" v-model:policyMeta="policyMetaModel.canCreateChannel" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canCreateChannel }}</template>
			<template #valueText>{{ valuesModel.canCreateChannel ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canCreateChannel" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.driveCapacity, 'driveCapacityMb'])" v-model:policyMeta="policyMetaModel.driveCapacityMb" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.driveCapacity }}</template>
			<template #valueText>{{ valuesModel.driveCapacityMb }}MB</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.driveCapacityMb" type="number" :disabled="disabled">
					<template #suffix>MB</template>
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.maxFileSize, 'maxFileSizeMb'])" v-model:policyMeta="policyMetaModel.maxFileSizeMb" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.maxFileSize }}</template>
			<template #valueText>{{ valuesModel.maxFileSizeMb }}MB</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.maxFileSizeMb" type="number" :disabled="disabled">
					<template #suffix>MB</template>
					<template #caption>
						<div>{{ i18n.tsx._role._options.maxFileSize_caption2({ max: `${Math.floor(instance.maxFileSize / (1024 * 1024))}MB` }) }}</div>
						<div><i class="ti ti-alert-triangle" style="color: var(--MI_THEME-warn);"></i> {{ i18n.ts._role._options.maxFileSize_caption }}</div>
					</template>
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.uploadableFileTypes, 'uploadableFileTypes'])" v-model:policyMeta="policyMetaModel.uploadableFileTypes" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.uploadableFileTypes }}</template>
			<template #valueText>...</template>
			<template #default="{ disabled }">
				<MkTextarea :modelValue="valuesModel.uploadableFileTypes.join('\n')" :disabled="disabled" @update:modelValue="v => valuesModel.uploadableFileTypes = v.split('\n')">
					<template #caption>
						<div>{{ i18n.ts._role._options.uploadableFileTypes_caption }}</div>
						<div><i class="ti ti-alert-triangle" style="color: var(--MI_THEME-warn);"></i> {{ i18n.tsx._role._options.uploadableFileTypes_caption2({ x: 'application/octet-stream' }) }}</div>
					</template>
				</MkTextarea>
			</template>
		</XFolder>

		<!--
			分割アップロード (#2313) の policy (#2900)。mk-go 独自。
			**集約は他の bool / 数値 policy と同じ** (bool は OR、数値は max)。
			intersection なのは下の optOutNotificationTypes だけ。
		-->
		<XFolder v-if="matchQuery([i18n.ts._mkgoRolePolicy.canRequestCustomEmojis, 'canRequestCustomEmojis'])" v-model:policyMeta="canRequestCustomEmojisMeta" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._mkgoRolePolicy.canRequestCustomEmojis }}</template>
			<template #valueText>{{ canRequestCustomEmojis ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="canRequestCustomEmojis" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
					<template #caption>{{ i18n.ts._mkgoRolePolicy.canRequestCustomEmojis_caption }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<!--
			カスタム絵文字をアバターデコレーションとして重ねられるか (#2975)。mk-go 独自。
			**個数の policy は無い** — 既存の avatarDecorationLimit に合算する。
		-->
		<XFolder v-if="matchQuery([i18n.ts._mkgoRolePolicy.canUseEmojiAsAvatarDecoration, 'canUseEmojiAsAvatarDecoration'])" v-model:policyMeta="canUseEmojiAsAvatarDecorationMeta" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._mkgoRolePolicy.canUseEmojiAsAvatarDecoration }}</template>
			<template #valueText>{{ canUseEmojiAsAvatarDecoration ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="canUseEmojiAsAvatarDecoration" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
					<template #caption>{{ i18n.ts._mkgoRolePolicy.canUseEmojiAsAvatarDecoration_caption }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<!--
			IP アドレスから関連アカウントを引く権限 (#3104)。mk-go 独自。
			**既定 false = 管理者のみ。** upstream の `admin/get-user-ips` は
			requireAdmin なので、既定でモデレーターへ開くと同じ機密情報に
			既存より緩い経路を新設することになる。開くかどうかは運営者が決める。
		-->
		<XFolder v-if="matchQuery([i18n.ts._mkgoRolePolicy.canSearchIpHistory, 'canSearchIpHistory'])" v-model:policyMeta="canSearchIpHistoryMeta" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._mkgoRolePolicy.canSearchIpHistory }}</template>
			<template #valueText>{{ canSearchIpHistory ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="canSearchIpHistory" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
					<template #caption>{{ i18n.ts._mkgoRolePolicy.canSearchIpHistory_caption }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._mkgoRolePolicy.canDeleteAccount, 'canDeleteAccount'])" v-model:policyMeta="canDeleteAccountMeta" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._mkgoRolePolicy.canDeleteAccount }}</template>
			<template #valueText>{{ canDeleteAccount ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="canDeleteAccount" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
					<template #caption>{{ i18n.ts._mkgoRolePolicy.canDeleteAccount_caption }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._mkgoRolePolicy.canPurgeAccount, 'canPurgeAccount'])" v-model:policyMeta="canPurgeAccountMeta" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._mkgoRolePolicy.canPurgeAccount }}</template>
			<template #valueText>{{ canPurgeAccount ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="canPurgeAccount" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
					<template #caption>{{ i18n.ts._mkgoRolePolicy.canPurgeAccount_caption }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._mkgoRolePolicy.canUseChunkedUpload, 'canUseChunkedUpload'])" v-model:policyMeta="canUseChunkedUploadMeta" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._mkgoRolePolicy.canUseChunkedUpload }}</template>
			<template #valueText>{{ canUseChunkedUpload ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="canUseChunkedUpload" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
					<template #caption>{{ i18n.ts._mkgoRolePolicy.canUseChunkedUpload_caption }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._mkgoRolePolicy.chunkedUploadMaxConcurrentSessions, 'chunkedUploadMaxConcurrentSessions'])" v-model:policyMeta="chunkedUploadMaxConcurrentSessionsMeta" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._mkgoRolePolicy.chunkedUploadMaxConcurrentSessions }}</template>
			<template #valueText>{{ chunkedUploadMaxConcurrentSessions }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="chunkedUploadMaxConcurrentSessions" type="number" :min="0" :disabled="disabled">
					<template #caption>{{ i18n.ts._mkgoRolePolicy.chunkedUploadLimits_caption }}</template>
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._mkgoRolePolicy.chunkedUploadMaxPendingMb, 'chunkedUploadMaxPendingMb'])" v-model:policyMeta="chunkedUploadMaxPendingMbMeta" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._mkgoRolePolicy.chunkedUploadMaxPendingMb }}</template>
			<template #valueText>{{ chunkedUploadMaxPendingMb }}MB</template>
			<template #default="{ disabled }">
				<MkInput v-model="chunkedUploadMaxPendingMb" type="number" :min="0" :disabled="disabled">
					<template #suffix>MB</template>
					<template #caption>{{ i18n.ts._mkgoRolePolicy.chunkedUploadLimits_caption }}</template>
				</MkInput>
			</template>
		</XFolder>

		<!--
			mk-go 固有 (#2958)。カスタム絵文字の申請をローリング期間で絞る。
			**固定の暦ではなく直前 N 時間で数える** — 暦だと切り替わりの直前と
			直後に連続で出せてしまう。0 はその期間の上限なし。
		-->
		<XFolder v-if="matchQuery([i18n.ts._mkgoRolePolicy.emojiApplicationMaxPerDay, 'emojiApplicationMaxPerDay'])" v-model:policyMeta="emojiApplicationMaxPerDayMeta" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._mkgoRolePolicy.emojiApplicationMaxPerDay }}</template>
			<template #valueText>{{ emojiApplicationMaxPerDay === 0 ? i18n.ts._mkgoRolePolicy.emojiApplicationQuotaUnlimited : emojiApplicationMaxPerDay }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="emojiApplicationMaxPerDay" type="number" :min="0" :disabled="disabled">
					<template #caption>{{ i18n.ts._mkgoRolePolicy.emojiApplicationQuota_caption }}</template>
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._mkgoRolePolicy.emojiApplicationMaxPerWeek, 'emojiApplicationMaxPerWeek'])" v-model:policyMeta="emojiApplicationMaxPerWeekMeta" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._mkgoRolePolicy.emojiApplicationMaxPerWeek }}</template>
			<template #valueText>{{ emojiApplicationMaxPerWeek === 0 ? i18n.ts._mkgoRolePolicy.emojiApplicationQuotaUnlimited : emojiApplicationMaxPerWeek }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="emojiApplicationMaxPerWeek" type="number" :min="0" :disabled="disabled">
					<template #caption>{{ i18n.ts._mkgoRolePolicy.emojiApplicationQuota_caption }}</template>
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._mkgoRolePolicy.emojiApplicationMaxPerMonth, 'emojiApplicationMaxPerMonth'])" v-model:policyMeta="emojiApplicationMaxPerMonthMeta" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._mkgoRolePolicy.emojiApplicationMaxPerMonth }}</template>
			<template #valueText>{{ emojiApplicationMaxPerMonth === 0 ? i18n.ts._mkgoRolePolicy.emojiApplicationQuotaUnlimited : emojiApplicationMaxPerMonth }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="emojiApplicationMaxPerMonth" type="number" :min="0" :disabled="disabled">
					<template #caption>{{ i18n.ts._mkgoRolePolicy.emojiApplicationQuota_caption }}</template>
				</MkInput>
			</template>
		</XFolder>

		<!--
			mk-go 固有 (#2977)。同時に審査待ちにできる件数。**期間の上限 (#2958) とは
			数え方が逆**で、却下・取り下げ・承認で枠が戻る。絞っているものも違い、
			あちらは「出せる総量」、こちらは「モデレーターが見る一覧の長さ」。
		-->
		<XFolder v-if="matchQuery([i18n.ts._mkgoRolePolicy.emojiApplicationMaxPending, 'emojiApplicationMaxPending'])" v-model:policyMeta="emojiApplicationMaxPendingMeta" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._mkgoRolePolicy.emojiApplicationMaxPending }}</template>
			<template #valueText>{{ emojiApplicationMaxPending === 0 ? i18n.ts._mkgoRolePolicy.emojiApplicationQuotaUnlimited : emojiApplicationMaxPending }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="emojiApplicationMaxPending" type="number" :min="0" :disabled="disabled">
					<template #caption>{{ i18n.ts._mkgoRolePolicy.emojiApplicationMaxPending_caption }}</template>
				</MkInput>
			</template>
		</XFolder>

		<!--
			mk-go 固有 (#2898)。ロール単位で通知を切る。**集約は intersection** なので、
			複数のロールに属している利用者は全ロールで切られている種類だけが届かなくなる
			(他の policy と同じく緩い方に倒す)。
		-->
		<XFolder v-if="matchQuery([i18n.ts._mkgoNotification.optOutNotificationTypes, 'optOutNotificationTypes'])" v-model:policyMeta="optOutPolicyMeta" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._mkgoNotification.optOutNotificationTypes }}</template>
			<template #valueText>{{ optOutTypes.length === 0 ? i18n.ts.none : optOutTypes.length }}</template>
			<template #default="{ disabled }">
				<div class="_gaps_s">
					<MkSwitch
						v-for="type in mkGoOptOutTargetTypes"
						:key="type"
						:modelValue="optOutTypes.includes(type)"
						:disabled="disabled"
						@update:modelValue="v => toggleOptOutNotificationType(type, v)"
					>
						{{ mkGoNotificationTypeLabel(type) }}
						<!--
							**届く相手が限られる型は明記する (#2868)。** この folder は
							全てのロールに出るので、権限を持たないロールでも「切れる」
							ように見え、通報の通知を一般利用者も受け取ると誤解される。
						-->
						<template v-if="type === 'abuseReport' || type === 'signupApplicationReceived'" #caption>{{ i18n.ts._mkgoNotification.abuseReportModeratorOnly }}</template>
						<!--
							**絵文字の申請だけ届く相手が違う (#2987)。** 審査は
							`canManageCustomEmojis` でできるので、モデレーターかどうかでは
							決まらない。同じ注意書きを出すと逆に誤解させる。
						-->
						<template v-else-if="type === 'emojiApplicationReceived'" #caption>{{ i18n.ts._mkgoNotification.emojiApplicationReviewerOnly }}</template>
					</MkSwitch>
					<MkInfo>{{ i18n.ts._mkgoNotification.optOutNotificationTypes_caption }}</MkInfo>
				</div>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.alwaysMarkNsfw, 'alwaysMarkNsfw'])" v-model:policyMeta="policyMetaModel.alwaysMarkNsfw" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.alwaysMarkNsfw }}</template>
			<template #valueText>{{ valuesModel.alwaysMarkNsfw ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.alwaysMarkNsfw" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canUpdateBioMedia, 'canUpdateBioMedia'])" v-model:policyMeta="policyMetaModel.canUpdateBioMedia" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canUpdateBioMedia }}</template>
			<template #valueText>{{ valuesModel.canUpdateBioMedia ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canUpdateBioMedia" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.pinMax, 'pinLimit'])" v-model:policyMeta="policyMetaModel.pinLimit" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.pinMax }}</template>
			<template #valueText>{{ valuesModel.pinLimit }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.pinLimit" type="number" :disabled="disabled">
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.antennaMax, 'antennaLimit'])" v-model:policyMeta="policyMetaModel.antennaLimit" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.antennaMax }}</template>
			<template #valueText>{{ valuesModel.antennaLimit }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.antennaLimit" type="number" :disabled="disabled">
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.wordMuteMax, 'wordMuteLimit'])" v-model:policyMeta="policyMetaModel.wordMuteLimit" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.wordMuteMax }}</template>
			<template #valueText>{{ valuesModel.wordMuteLimit }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.wordMuteLimit" type="number" :disabled="disabled">
					<template #suffix>chars</template>
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.webhookMax, 'webhookLimit'])" v-model:policyMeta="policyMetaModel.webhookLimit" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.webhookMax }}</template>
			<template #valueText>{{ valuesModel.webhookLimit }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.webhookLimit" type="number" :disabled="disabled">
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.clipMax, 'clipLimit'])" v-model:policyMeta="policyMetaModel.clipLimit" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.clipMax }}</template>
			<template #valueText>{{ valuesModel.clipLimit }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.clipLimit" type="number" :disabled="disabled">
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.noteEachClipsMax, 'noteEachClipsLimit'])" v-model:policyMeta="policyMetaModel.noteEachClipsLimit" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.noteEachClipsMax }}</template>
			<template #valueText>{{ valuesModel.noteEachClipsLimit }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.noteEachClipsLimit" type="number" :disabled="disabled">
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.userListMax, 'userListLimit'])" v-model:policyMeta="policyMetaModel.userListLimit" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.userListMax }}</template>
			<template #valueText>{{ valuesModel.userListLimit }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.userListLimit" type="number" :disabled="disabled">
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.userEachUserListsMax, 'userEachUserListsLimit'])" v-model:policyMeta="policyMetaModel.userEachUserListsLimit" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.userEachUserListsMax }}</template>
			<template #valueText>{{ valuesModel.userEachUserListsLimit }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.userEachUserListsLimit" type="number" :disabled="disabled">
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canHideAds, 'canHideAds'])" v-model:policyMeta="policyMetaModel.canHideAds" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canHideAds }}</template>
			<template #valueText>{{ valuesModel.canHideAds ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canHideAds" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.avatarDecorationLimit, 'avatarDecorationLimit'])" v-model:policyMeta="policyMetaModel.avatarDecorationLimit" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.avatarDecorationLimit }}</template>
			<template #valueText>{{ valuesModel.avatarDecorationLimit }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="avatarDecorationLimit" type="number" :disabled="disabled" :min="0" :max="16" @update:modelValue="updateAvatarDecorationLimit">
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canImportAntennas, 'canImportAntennas'])" v-model:policyMeta="policyMetaModel.canImportAntennas" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canImportAntennas }}</template>
			<template #valueText>{{ valuesModel.canImportAntennas ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canImportAntennas" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canImportBlocking, 'canImportBlocking'])" v-model:policyMeta="policyMetaModel.canImportBlocking" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canImportBlocking }}</template>
			<template #valueText>{{ valuesModel.canImportBlocking ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canImportBlocking" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canImportFollowing, 'canImportFollowing'])" v-model:policyMeta="policyMetaModel.canImportFollowing" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canImportFollowing }}</template>
			<template #valueText>{{ valuesModel.canImportFollowing ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canImportFollowing" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canImportMuting, 'canImportMuting'])" v-model:policyMeta="policyMetaModel.canImportMuting" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canImportMuting }}</template>
			<template #valueText>{{ valuesModel.canImportMuting ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canImportMuting" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.canImportUserLists, 'canImportUserList'])" v-model:policyMeta="policyMetaModel.canImportUserLists" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.canImportUserLists }}</template>
			<template #valueText>{{ valuesModel.canImportUserLists ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.canImportUserLists" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.noteDraftLimit, 'noteDraftLimit'])" v-model:policyMeta="policyMetaModel.noteDraftLimit" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.noteDraftLimit }}</template>
			<template #valueText>{{ valuesModel.noteDraftLimit }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.noteDraftLimit" type="number" :disabled="disabled" :min="0">
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.scheduledNoteLimit, 'scheduledNoteLimit'])" v-model:policyMeta="policyMetaModel.scheduledNoteLimit" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.scheduledNoteLimit }}</template>
			<template #valueText>{{ valuesModel.scheduledNoteLimit }}</template>
			<template #default="{ disabled }">
				<MkInput v-model="valuesModel.scheduledNoteLimit" type="number" :disabled="disabled" :min="0">
				</MkInput>
			</template>
		</XFolder>

		<XFolder v-if="matchQuery([i18n.ts._role._options.watermarkAvailable, 'watermarkAvailable'])" v-model:policyMeta="policyMetaModel.watermarkAvailable" :isBaseRole="isBaseRole" :readonly="readonly">
			<template #label>{{ i18n.ts._role._options.watermarkAvailable }}</template>
			<template #valueText>{{ valuesModel.watermarkAvailable ? i18n.ts.yes : i18n.ts.no }}</template>
			<template #default="{ disabled }">
				<MkSwitch v-model="valuesModel.watermarkAvailable" :disabled="disabled">
					<template #label>{{ i18n.ts.enable }}</template>
				</MkSwitch>
			</template>
		</XFolder>
	</div>
</template>

<script lang="ts">
import * as Misskey from 'misskey-js';
import { instance } from '@/instance.js';

export type PolicyMeta = {
	useDefault: boolean;
	priority: number;
};

type PolicyMetaRecord = {
	[K in keyof Misskey.entities.RolePolicies]: PolicyMeta;
};
</script>

<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import { i18n } from '@/i18n.js';
import XFolder from './roles.policy-editor.folder.vue';

import MkInput from '@/components/MkInput.vue';
import MkTextarea from '@/components/MkTextarea.vue';
import MkRange from '@/components/MkRange.vue';
import MkSwitch from '@/components/MkSwitch.vue';
import MkSelect from '@/components/MkSelect.vue';
import MkInfo from '@/components/MkInfo.vue';
import { accountDeletionPolicyDefaults } from '@/utility/account-delete-policy.js';

/**
 * Notification types the opt-out policy can target (#2898).
 *
 * mk-go 固有の型 (abuseReport) を含むので misskey-js の notificationTypes を
 * そのまま使えない。**利用者が自分では切れない型に絞っている** — 個人設定の
 * notificationRecieveConfig で切れるものはそちらでよく、ロールで一括して切りたいのは
 * 運営向けに配られる通知のほう。
 */
const mkGoOptOutTargetTypes = ['abuseReport', 'emojiApplicationReceived', 'signupApplicationReceived'] as const;

const props = defineProps<{
	isBaseRole: boolean;
	rolePolicies: Misskey.entities.RolePolicies;
	policiesMeta?: PolicyMetaRecord;
	roleQuery?: string;
	readonly?: boolean;
}>();

const emit = defineEmits<{
	(event: 'update:rolePolicies', value: Misskey.entities.RolePolicies): void;
	(event: 'update:policiesMeta', value: PolicyMetaRecord): void;
}>();

const valuesModel = ref(props.rolePolicies);
watch(valuesModel, (newVal) => {
	emit('update:rolePolicies', newVal);
}, { deep: true });
watch(() => props.rolePolicies, () => {
	valuesModel.value = props.rolePolicies;
}, { deep: true });

/**
 * Policy keys this editor tracks metadata for, including mk-go specific ones.
 *
 * **misskey-js の rolePolicies だけでは足りない (#2898)。** 固有キーの meta が
 * 毎回落ちると、保存済みの override を開き直したときに「ベース値を使用」と
 * 表示されスイッチが無効になる。さらに再編集のため useDefault を off にすると、
 * 捏造された `{useDefault:true, priority:0}` が起点になり**保存済みの priority が
 * 黙って 0 に戻る**。
 *
 * roles.editor.vue の同名の一覧と対になっている。片方だけ直すと同じ症状が残る。
 */
const mkGoPolicyMetaKeys: string[] = [
	...Misskey.rolePolicies,
	'optOutNotificationTypes',
	'canRequestCustomEmojis',
	'canUseEmojiAsAvatarDecoration',
	'canSearchIpHistory',
	'canDeleteAccount',
	'canPurgeAccount',
	'canUseChunkedUpload',
	'chunkedUploadMaxConcurrentSessions',
	'chunkedUploadMaxPendingMb',
	'emojiApplicationMaxPerDay',
	'emojiApplicationMaxPerWeek',
	'emojiApplicationMaxPerMonth',
	'emojiApplicationMaxPending',
];

function setPolicyMeta(incoming: Partial<PolicyMetaRecord> | undefined): PolicyMetaRecord {
	const meta = {} as Record<string, PolicyMeta>;
	const src = (incoming ?? {}) as Record<string, PolicyMeta | undefined>;
	for (const ROLE_POLICY of mkGoPolicyMetaKeys) {
		meta[ROLE_POLICY] = src[ROLE_POLICY] ?? {
			useDefault: true,
			priority: 0,
		};
	}
	return meta as PolicyMetaRecord;
}

const policyMetaModel = ref(setPolicyMeta(props.policiesMeta));
watch(policyMetaModel, (newVal) => {
	emit('update:policiesMeta', newVal);
}, { deep: true });
watch(() => props.policiesMeta, () => {
	policyMetaModel.value = setPolicyMeta(props.policiesMeta);
}, { deep: true });

/**
 * mk-go 独自 policy の値を読み書きする (#2898 / #2900)。
 *
 * **autogen を書き換えない。** あちらは openapi から再生成されるので、足しても
 * 次の生成で消える。mk-go 独自 endpoint を `as never` で呼ぶのと同じ扱い。
 *
 * キーが増えるたびに computed を手書きすると、キャストが散らばって片側だけ
 * 直す形の穴ができる。読み書きはここに閉じ込める。
 */
function mkGoPolicyValue<T>(key: string, fallback: T) {
	return computed<T>({
		get: () => {
			const v = (valuesModel.value as unknown as Record<string, unknown>)[key];
			return (v === undefined || v === null ? fallback : v) as T;
		},
		set: (v) => {
			(valuesModel.value as unknown as Record<string, unknown>)[key] = v;
		},
	});
}

/** mk-go 独自 policy の meta (useDefault / priority) を読み書きする。 */
function mkGoPolicyMeta(key: string) {
	return computed<PolicyMeta>({
		get: () => (policyMetaModel.value as unknown as Record<string, PolicyMeta | undefined>)[key] ?? { useDefault: true, priority: 0 },
		set: (v) => {
			(policyMetaModel.value as unknown as Record<string, PolicyMeta | undefined>)[key] = v;
		},
	});
}

const optOutTypes = computed<string[]>(() => {
	const v = (valuesModel.value as unknown as Record<string, unknown>).optOutNotificationTypes;
	return Array.isArray(v) ? v as string[] : [];
});

const optOutPolicyMeta = mkGoPolicyMeta('optOutNotificationTypes');

// 分割アップロード (#2313) の policy (#2900)。既定は backend の
// internal/effectivepolicy/validation.go と揃える。
// 絵文字の登録申請 (#2934)。既定 true — 登録は必ず承認を通るので、申請
// そのものを既定で塞がない。effectivepolicy の default と揃えること。
const canRequestCustomEmojis = mkGoPolicyValue('canRequestCustomEmojis', true);
const canRequestCustomEmojisMeta = mkGoPolicyMeta('canRequestCustomEmojis');
const canUseEmojiAsAvatarDecoration = mkGoPolicyValue('canUseEmojiAsAvatarDecoration', true);
const canUseEmojiAsAvatarDecorationMeta = mkGoPolicyMeta('canUseEmojiAsAvatarDecoration');
// IP からの関連アカウント検索 (#3104)。**既定 false** — 管理者のみ。
// internal/effectivepolicy/validation.go の default と揃えること。
const canSearchIpHistory = mkGoPolicyValue('canSearchIpHistory', false);
const canSearchIpHistoryMeta = mkGoPolicyMeta('canSearchIpHistory');
// Keep these defaults aligned with shiroha-a/mk
// internal/effectivepolicy/validation.go (backend PR #3316).
const canDeleteAccount = mkGoPolicyValue('canDeleteAccount', accountDeletionPolicyDefaults.canDeleteAccount);
const canDeleteAccountMeta = mkGoPolicyMeta('canDeleteAccount');
const canPurgeAccount = mkGoPolicyValue('canPurgeAccount', accountDeletionPolicyDefaults.canPurgeAccount);
const canPurgeAccountMeta = mkGoPolicyMeta('canPurgeAccount');
const canUseChunkedUpload = mkGoPolicyValue('canUseChunkedUpload', true);
const canUseChunkedUploadMeta = mkGoPolicyMeta('canUseChunkedUpload');
const chunkedUploadMaxConcurrentSessions = mkGoPolicyValue('chunkedUploadMaxConcurrentSessions', 4);
const chunkedUploadMaxConcurrentSessionsMeta = mkGoPolicyMeta('chunkedUploadMaxConcurrentSessions');
const chunkedUploadMaxPendingMb = mkGoPolicyValue('chunkedUploadMaxPendingMb', 1024);
const chunkedUploadMaxPendingMbMeta = mkGoPolicyMeta('chunkedUploadMaxPendingMb');

// カスタム絵文字申請の期間上限 (#2958)。既定はいずれも 0 = 無制限。
// internal/effectivepolicy/validation.go と揃えること。
const emojiApplicationMaxPerDay = mkGoPolicyValue('emojiApplicationMaxPerDay', 0);
const emojiApplicationMaxPerDayMeta = mkGoPolicyMeta('emojiApplicationMaxPerDay');
const emojiApplicationMaxPerWeek = mkGoPolicyValue('emojiApplicationMaxPerWeek', 0);
const emojiApplicationMaxPerWeekMeta = mkGoPolicyMeta('emojiApplicationMaxPerWeek');
const emojiApplicationMaxPerMonth = mkGoPolicyValue('emojiApplicationMaxPerMonth', 0);
const emojiApplicationMaxPerMonthMeta = mkGoPolicyMeta('emojiApplicationMaxPerMonth');
// 審査待ちの上限 (#2977)。既定 0 = 無制限。
const emojiApplicationMaxPending = mkGoPolicyValue('emojiApplicationMaxPending', 0);
const emojiApplicationMaxPendingMeta = mkGoPolicyMeta('emojiApplicationMaxPending');

/**
 * Add or remove one notification type from the opt-out list (#2898).
 *
 * 参照を差し替えて deep watch を発火させる。
 */
function toggleOptOutNotificationType(type: string, enabled: boolean): void {
	const current = optOutTypes.value;
	const next = enabled
		? (current.includes(type) ? current : [...current, type])
		: current.filter(t => t !== type);
	(valuesModel.value as unknown as Record<string, unknown>).optOutNotificationTypes = next;
}

/**
 * Label for a notification type in the opt-out switch list.
 *
 * mk-go 固有の型は upstream の `_notification._types` に無いので、専用の文言へ
 * 落とす。どちらにも無ければ型名をそのまま出す (空ラベルより読める)。
 */
function mkGoNotificationTypeLabel(type: string): string {
	// **mk-go 固有の型は `_notification._types` に無い。** あそこは upstream の
	// 21 種だけなので、分岐を足さないと `table[type] ?? type` が**生の識別子**を
	// そのままラベルとして描く (全言語)。運営向け通知を足すたびにここも要る。
	if (type === 'abuseReport') return i18n.ts._mkgoNotification.abuseReport;
	if (type === 'emojiApplicationReceived') return i18n.ts._mkgoNotification.emojiApplicationReceivedLabel;
	if (type === 'signupApplicationReceived') return i18n.ts._mkgoNotification.signupApplicationReceived;
	const table = i18n.ts._notification._types as unknown as Record<string, string | undefined>;
	return table[type] ?? type;
}

function matchQuery(keywords: string[]): boolean {
	if (props.roleQuery == null || props.roleQuery.trim().length === 0) return true;
	return keywords.some(keyword => keyword.toLowerCase().includes(props.roleQuery!.toLowerCase()));
}

const avatarDecorationLimit = computed({
	get: () => Math.min(16, Math.max(0, Number(valuesModel.value.avatarDecorationLimit ?? 0))),
	set: (value) => {
		valuesModel.value.avatarDecorationLimit = Math.min(Number(value), 16);
	},
});

function updateAvatarDecorationLimit(value: string | number) {
	avatarDecorationLimit.value = Number(value);
}
</script>
