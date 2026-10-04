<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<PageWithHeader :tabs="headerTabs">
	<div class="_spacer" style="--MI_SPACER-w: 700px; --MI_SPACER-min: 16px; --MI_SPACER-max: 32px;">
		<SearchMarker path="/admin/object-storage" :label="i18n.ts.objectStorage" :keywords="['objectStorage']" icon="ti ti-cloud">
			<div class="_gaps_m">
				<SearchMarker>
					<MkSwitch v-model="useObjectStorage"><SearchLabel>{{ i18n.ts.useObjectStorage }}</SearchLabel></MkSwitch>
				</SearchMarker>

				<template v-if="useObjectStorage">
					<SearchMarker>
						<MkInput v-model="objectStorageBaseUrl" :placeholder="'https://example.com'" type="url">
							<template #label><SearchLabel>{{ i18n.ts.objectStorageBaseUrl }}</SearchLabel></template>
							<template #caption><SearchText>{{ i18n.ts.objectStorageBaseUrlDesc }}</SearchText></template>
						</MkInput>
					</SearchMarker>

					<SearchMarker>
						<MkInput v-model="objectStorageBucket">
							<template #label><SearchLabel>{{ i18n.ts.objectStorageBucket }}</SearchLabel></template>
							<template #caption><SearchText>{{ i18n.ts.objectStorageBucketDesc }}</SearchText></template>
						</MkInput>
					</SearchMarker>

					<SearchMarker>
						<MkInput v-model="objectStoragePrefix">
							<template #label><SearchLabel>{{ i18n.ts.objectStoragePrefix }}</SearchLabel></template>
							<template #caption><SearchText>{{ i18n.ts.objectStoragePrefixDesc }}</SearchText></template>
						</MkInput>
					</SearchMarker>

					<SearchMarker>
						<MkInput v-model="objectStorageEndpoint" :placeholder="'example.com'">
							<template #label><SearchLabel>{{ i18n.ts.objectStorageEndpoint }}</SearchLabel></template>
							<template #prefix>https://</template>
							<template #caption><SearchText>{{ i18n.ts.objectStorageEndpointDesc }}</SearchText></template>
						</MkInput>
					</SearchMarker>

					<SearchMarker>
						<MkInput v-model="objectStorageRegion">
							<template #label><SearchLabel>{{ i18n.ts.objectStorageRegion }}</SearchLabel></template>
							<template #caption><SearchText>{{ i18n.ts.objectStorageRegionDesc }}</SearchText></template>
						</MkInput>
					</SearchMarker>

					<FormSplit :minWidth="280">
						<SearchMarker>
							<MkInput v-model="objectStorageAccessKey">
								<template #prefix><i class="ti ti-key"></i></template>
								<template #label><SearchLabel>Access key</SearchLabel></template>
							</MkInput>
						</SearchMarker>

						<SearchMarker>
							<MkInput v-model="objectStorageSecretKey" type="password" autocomplete="new-password">
								<template #prefix><i class="ti ti-key"></i></template>
								<template #label><SearchLabel>Secret key</SearchLabel></template>
							</MkInput>
						</SearchMarker>
					</FormSplit>

					<SearchMarker>
						<MkSwitch v-model="objectStorageUseSSL">
							<template #label><SearchLabel>{{ i18n.ts.objectStorageUseSSL }}</SearchLabel></template>
							<template #caption><SearchText>{{ i18n.ts.objectStorageUseSSLDesc }}</SearchText></template>
						</MkSwitch>
					</SearchMarker>

					<SearchMarker>
						<MkSwitch v-model="objectStorageUseProxy">
							<template #label><SearchLabel>{{ i18n.ts.objectStorageUseProxy }}</SearchLabel></template>
							<template #caption><SearchText>{{ i18n.ts.objectStorageUseProxyDesc }}</SearchText></template>
						</MkSwitch>
					</SearchMarker>

					<SearchMarker>
						<MkSwitch v-model="objectStorageSetPublicRead">
							<template #label><SearchLabel>{{ i18n.ts.objectStorageSetPublicRead }}</SearchLabel></template>
						</MkSwitch>
					</SearchMarker>

					<SearchMarker>
						<MkSwitch v-model="objectStorageS3ForcePathStyle">
							<template #label><SearchLabel>s3ForcePathStyle</SearchLabel></template>
							<template #caption><SearchText>{{ i18n.ts.s3ForcePathStyleDesc }}</SearchText></template>
						</MkSwitch>
					</SearchMarker>

					<!-- 分割アップロード (mk-go 独自)。マルチパートアップロードに
					     依存するのでオブジェクトストレージ有効時のみ意味を持つ。
					     純正 backend では admin/meta に該当 field が無いので
					     未定義判定で丸ごと隠す。 -->
					<template v-if="chunkedUploadSupported">
						<MkFolder>
							<template #label><SearchLabel>{{ i18n.ts._chunkedUpload.enable }}</SearchLabel></template>
							<template #suffix>{{ chunkedUploadEnabled ? i18n.ts.enabled : i18n.ts.disabled }}</template>

							<div class="_gaps_m">
								<SearchMarker>
									<MkSwitch v-model="chunkedUploadEnabled">
										<template #label><SearchLabel>{{ i18n.ts._chunkedUpload.enable }}</SearchLabel></template>
										<template #caption><SearchText>{{ i18n.ts._chunkedUpload.description }}</SearchText></template>
									</MkSwitch>
								</SearchMarker>

								<MkInfo warn>{{ i18n.ts._chunkedUpload.chunkSizeDescription }}</MkInfo>

								<SearchMarker>
									<MkInput v-model="chunkedUploadChunkSizeMb" type="number" :min="5" :max="32">
										<template #label><SearchLabel>{{ i18n.ts._chunkedUpload.chunkSize }}</SearchLabel></template>
										<template #suffix>MiB</template>
									</MkInput>
								</SearchMarker>

								<SearchMarker>
									<MkInput v-model="chunkedUploadSessionTtlMinutes" type="number" :min="5" :max="1440">
										<template #label><SearchLabel>{{ i18n.ts._chunkedUpload.sessionTtl }}</SearchLabel></template>
										<template #caption><SearchText>{{ i18n.ts._chunkedUpload.sessionTtlDescription }}</SearchText></template>
									</MkInput>
								</SearchMarker>
							</div>
						</MkFolder>
					</template>
				</template>

				<!-- オブジェクトストレージ無効時は分割アップロードも使えない旨を出す。 -->
				<MkInfo v-else-if="chunkedUploadSupported">{{ i18n.ts._chunkedUpload.requiresObjectStorage }}</MkInfo>
			</div>
		</SearchMarker>
	</div>
	<template #footer>
		<div :class="$style.footer">
			<div class="_spacer" style="--MI_SPACER-w: 700px; --MI_SPACER-min: 16px; --MI_SPACER-max: 16px;">
				<MkButton primary rounded @click="save"><i class="ti ti-check"></i> {{ i18n.ts.save }}</MkButton>
			</div>
		</div>
	</template>
</PageWithHeader>
</template>

<script lang="ts" setup>
import { ref, computed } from 'vue';
import MkSwitch from '@/components/MkSwitch.vue';
import MkInput from '@/components/MkInput.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import FormSplit from '@/components/form/split.vue';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { fetchInstance } from '@/instance.js';
import { i18n } from '@/i18n.js';
import { definePage } from '@/page.js';
import MkButton from '@/components/MkButton.vue';

const meta = await misskeyApi('admin/meta');

const useObjectStorage = ref(meta.useObjectStorage);
const objectStorageBaseUrl = ref(meta.objectStorageBaseUrl);
const objectStorageBucket = ref(meta.objectStorageBucket);
const objectStoragePrefix = ref(meta.objectStoragePrefix);
const objectStorageEndpoint = ref(meta.objectStorageEndpoint);
const objectStorageRegion = ref(meta.objectStorageRegion);
const objectStoragePort = ref(meta.objectStoragePort);
const objectStorageAccessKey = ref(meta.objectStorageAccessKey);
const objectStorageSecretKey = ref(meta.objectStorageSecretKey);
const objectStorageUseSSL = ref(meta.objectStorageUseSSL);
const objectStorageUseProxy = ref(meta.objectStorageUseProxy);
const objectStorageSetPublicRead = ref(meta.objectStorageSetPublicRead);
const objectStorageS3ForcePathStyle = ref(meta.objectStorageS3ForcePathStyle);

// 分割アップロード (mk-go 独自)。純正 backend の admin/meta には無いので、
// undefined 判定で UI ごと隠す。
const mkMeta = meta as typeof meta & {
	chunkedUploadEnabled?: boolean;
	chunkedUploadChunkSizeMb?: number;
	chunkedUploadSessionTtlMinutes?: number;
};
const chunkedUploadSupported = mkMeta.chunkedUploadEnabled !== undefined;
const chunkedUploadEnabled = ref(mkMeta.chunkedUploadEnabled ?? false);
const chunkedUploadChunkSizeMb = ref(mkMeta.chunkedUploadChunkSizeMb ?? 10);
const chunkedUploadSessionTtlMinutes = ref(mkMeta.chunkedUploadSessionTtlMinutes ?? 60);

function save() {
	os.apiWithDialog('admin/update-meta', {
		useObjectStorage: useObjectStorage.value,
		objectStorageBaseUrl: objectStorageBaseUrl.value,
		objectStorageBucket: objectStorageBucket.value,
		objectStoragePrefix: objectStoragePrefix.value,
		objectStorageEndpoint: objectStorageEndpoint.value,
		objectStorageRegion: objectStorageRegion.value,
		objectStoragePort: objectStoragePort.value,
		objectStorageAccessKey: objectStorageAccessKey.value,
		objectStorageSecretKey: objectStorageSecretKey.value,
		objectStorageUseSSL: objectStorageUseSSL.value,
		objectStorageUseProxy: objectStorageUseProxy.value,
		objectStorageSetPublicRead: objectStorageSetPublicRead.value,
		objectStorageS3ForcePathStyle: objectStorageS3ForcePathStyle.value,
		// 純正 backend には無い field なので、対応している場合だけ送る。
		...(chunkedUploadSupported ? {
			chunkedUploadEnabled: chunkedUploadEnabled.value,
			chunkedUploadChunkSizeMb: Number(chunkedUploadChunkSizeMb.value),
			chunkedUploadSessionTtlMinutes: Number(chunkedUploadSessionTtlMinutes.value),
		} : {}),
	}).then(() => {
		fetchInstance(true);
	});
}

const headerTabs = computed(() => []);

definePage(() => ({
	title: i18n.ts.objectStorage,
	icon: 'ti ti-cloud',
}));
</script>

<style lang="scss" module>
.footer {
	-webkit-backdrop-filter: var(--MI-blur, blur(15px));
	backdrop-filter: var(--MI-blur, blur(15px));
}
</style>
