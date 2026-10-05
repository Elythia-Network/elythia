.PHONY: help check gates version plugin-test frontend-check diff-check playwright-check e2e-down-all \
	update pull pull-plugins docker-update docker-rebuild docker-restart uds-update \
	image-up image-down image-down-v image-logs image-build \
	build run dev clean tidy test fmt lint plugin-doc-check migrate-up migrate-down migrate-create \
	plugins plugins-all plugin-dev plugin-vet \
	federation-misskey-build federation-misskey-up federation-misskey-test \
	federation-misskey-e2e \
	federation-misskey-down federation-misskey-logs \
	federation-mastodon-e2e federation-mastodon-down \
	dropin-up dropin-down dropin-test dropin-logs \
	dropin-mk-up dropin-mk-test dropin-mk-down dropin-mk-logs dropin-swap-test dropin-fedibird-test \
	dropin-mkgo-born-test \
	dropin-frontend-up dropin-frontend-down dropin-frontend-baseline dropin-frontend-logs \
	dropin-frontend-mk-up dropin-frontend-mk-down dropin-frontend-swap-test \
	e2e-frontend-build \
	uds-init uds-layout-check uds-frontend-build uds-build uds-rebuild uds-restart uds-up uds-down uds-down-v uds-logs uds-ps \
	bench-up bench-run bench-down bench-logs \
	apicompat apicompat-routes apicompat-render \
	test-fast shapecheck shapecheck-gen shapecheck-report errorid-check limitspec-check perm-check wiring-check catalog-check notfound-check nulparam-check compose-check testflags-check gaterun-check secretfield-check ipshape-check iprecord-check \
	diff-up diff-test diff-down diff-logs \
	upstream-e2e upstream-e2e-deps upstream-e2e-up upstream-e2e-down upstream-e2e-migrate upstream-e2e-test

.DEFAULT_GOAL := help

##@ ヘルプ

help: ## この一覧を表示 (引数なしの make でも出る)
	@awk 'BEGIN {FS = ":.*##"} \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5); next } \
		/^[a-zA-Z0-9_.-]+:.*##/ { printf "  \033[36m%-30s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@printf "\n各 e2e / ベンチの詳細は docs/development.md を参照。\n"

##@ まとめて実行

check: fmt lint actionlint golangci-lint test ## コミット前に必須 (lint job の静的検査 + test)
	# **`lint` job と揃える。** あちらは go vet だけでなく actionlint と
	# golangci-lint も回すので、`fmt`/`lint`/`test` だけだと新しい検査が手元で
	# 一度も走らない (レビューで指摘された)。
	#
	# **required check の全部ではない。** `build` job (`go build ./...` と同梱
	# プラグインの vet → `make plugin-vet`、同梱サンプル入りの統合バイナリ →
	# `make plugins-all && go build ./cmd/elythia`)、`lint` job の重複 fixture ID 検査、
	# `test` のカバレッジ閾値は再現しない。

gates: shapecheck errorid-check limitspec-check perm-check wiring-check catalog-check notfound-check nulparam-check compose-check testflags-check migrationdoc-check mdtable-check notiftype-check pluginembed-check dockerignore-check secretfield-check ipshape-check iprecord-check sqlbind-check gaterun-check ## 静的 parity ゲートを一括実行

version: ## mk-go / 互換 Misskey / 追従している本家のバージョンを表示
	@printf "mk-go            : %s\n" "$$(sed -n 's/^var MkGoVersion = "\(.*\)"/\1/p' internal/config/config.go)"
	@printf "互換 Misskey     : %s\n" "$$(sed -n 's/^var MisskeyVersion = "\(.*\)"/\1/p' internal/config/config.go)"
	@printf "追従している本家 : %s\n" "$$(cat UPSTREAM_MISSKEY_VERSION 2>/dev/null || echo '(不明)')"

# frontend-check / frontend-lint / frontend-test は、frontend が import する
# pluginbuild の生成物を前提にする (追跡していない、#3379)。無いと vue-tsc などが
# 解決できずに落ちる。**無いときだけ作る。** `plugins` を前提にすると、CI が先に `make plugins-all`
# (既定無効の同梱サンプルを含める、#2495) で作ったものを、サンプル抜きで
# 書き直してしまう。plugin-api.ts を壊してもサンプル側で検出されなくなる。
FRONTEND_PLUGINS_GENERATED = frontend/packages/frontend/src/server-plugins.generated.ts

$(FRONTEND_PLUGINS_GENERATED):
	GOWORK=off go run ./tools/pluginbuild

frontend-check: | $(FRONTEND_PLUGINS_GENERATED) ## frontend/ を型チェックし、frontend を読むゲートを回す
	# uds-frontend-build / e2e-frontend-build (と frontend/ での pnpm build /
	# pnpm -r build) は本番が bind-mount している frontend/built を書き換えるため、
	# 本番のチェックアウトで検証目的に使わないこと。
	# 型を見るだけならこちらで済む (Docker 不要、出力物も作らない)。
	cd frontend/packages/frontend && npx vue-tsc --noEmit
	# frontend/ のソースを読むゲート。frontend/ を本体で追跡するようになった (#3379)
	# ので skip せず、required の `test` でも走る。frontend を触ったときに手元で
	# まとめて回せるよう、ここにも残す。
	go test ./internal/server/ \
		-run 'TestCreditImageOriginsCoverAboutMisskey|TestMkGoRolePolicyKeysAreListedInFrontend|TestReactionLongPressIsWired|TestReactableRemoteReactionIsWired|TestMkGoUpdatedDialogIsWired|TestEmojiApplicationIsWired|TestEveryPluginSlotHasAMountPoint|TestAutoLoadingComponentsShowRateLimit|TestRemoteImagesGoThroughMediaProxy|TestRemoteImageProxyGateClassifiesSources|TestEmojiDecorationErrorIDsMatchFrontend|TestStaffNotificationTypesAreOptOutable|TestNotificationBadgeClassesHaveNoPadding|TestEmojiRequestEntriesUseTheSharedHelper|TestCSSModulesHaveNoDuplicateClasses|TestCleanRemoteFilesButtonIsConditional|TestStreamResyncIsWiredInTimelines' -count=1
	# MFM の Unicode 絵文字の正規表現と記録した版が、frontend の mfm-js / emoji-data と一致するか
	# (#3324)。node_modules が無いと落ちる (skip しない)。
	$(MAKE) emoji-regex-check
	# **eslint も回す (#2906)。** CI は別 step で `pnpm eslint` を回しており、
	# ここに無いと**手元で緑でも CI が落ちる**。#2903 で実際に踏んだ (デッドコードを
	# 消したときの空行が @stylistic/no-multiple-empty-lines で落ちた)。個別ファイルに
	# `npx eslint` を掛けても CI と同じ glob ではないので見落とす。
	#
	# **script を呼ぶ (引数を書き写さない)。** 書き写すと package.json と
	# ドリフトする (#2841 の `make test` と CI の flag が同じ形でずれた)。
	$(MAKE) frontend-lint

.PHONY: frontend-lint
frontend-lint: | $(FRONTEND_PLUGINS_GENERATED) ## frontend/ の frontend を eslint で検査 (CI と同じ範囲)
	# CI の `Lint (eslint)` step と同じ。範囲は package.json の script が持つ
	# (`--quiet "src/**/*.{ts,vue}"`)。**`eslint .` にしないこと** — upstream が
	# lint していない test/ まで拾い、追従のたびに他人の負債で落ちる。
	#
	# **`npm run` で script を呼ぶ (引数を書き写さない)。** 書き写すと
	# package.json とドリフトする (#2841 の `make test` と CI の flag が同じ形で
	# ずれた)。CI は `pnpm eslint` だが手元に pnpm があるとは限らないので、
	# 同じ script を呼べる npx/npm 側に寄せる (frontend-check / frontend-test も
	# npx を使っている)。
	cd frontend/packages/frontend && npm run --silent eslint

.PHONY: frontend-test
frontend-test: | $(FRONTEND_PLUGINS_GENERATED) ## frontend/ の frontend の vitest を実行
	# upstream の `pnpm --filter frontend test` と同じ。CI は frontend workflow で
	# 回す (#3379)。workspace package の生成物が要るので、初回や本家の版を上げた後は
	# 先に `cd frontend && pnpm install && pnpm build` (plugins は前提として走る)。
	# この pnpm build は frontend/built を作り直すので、本番のチェックアウトでは流さない。
	cd frontend/packages/frontend && npx vitest --run --globals --config vitest.config.unit.ts

diff-check: ## 差分比較ハーネスを作り直して実行 (クリーン DB 前提)
	$(MAKE) diff-down
	$(MAKE) diff-up
	@printf "backend の healthy 待ち...\n"
	@until [ "$$(docker inspect mkdiff-mkgo-1 --format '{{.State.Health.Status}}' 2>/dev/null)" = healthy ] \
		&& [ "$$(docker inspect mkdiff-ts-1 --format '{{.State.Health.Status}}' 2>/dev/null)" = healthy ]; do sleep 3; done
	$(MAKE) diff-test

playwright-check: ## Playwright を作り直して実行 (クリーン DB 前提)
	$(MAKE) playwright-down
	$(MAKE) playwright-up
	@printf "backend の healthy 待ち...\n"
	@until [ "$$(docker inspect mk-playwright-mkgo-1 --format '{{.State.Health.Status}}' 2>/dev/null)" = healthy ]; do sleep 3; done
	$(MAKE) playwright-test

# 検証用スタックだけを撤去する。**docker-compose.yml は含めない** —
# あちらは name: を持たないため project 名がディレクトリ名 (= mk) になり、
# 同名で動いている本番スタックを巻き込む。ここでは name: を明示している
# 検証用 compose ファイルだけを列挙する。
E2E_COMPOSE_FILES = \
	docker-compose.image.yml \
	tests/diff/compose.yml \
	tests/playwright/compose.yml \
	tests/dropin/compose.yml \
	tests/dropin-frontend/compose.yml \
	tests/federation/compose.misskey.yml \
	tests/federation/compose.mastodon.yml \
	tests/bench/http/compose.yml \
	tests/bench/queue/compose.yml

# 使われている profile を全部渡す。**`down` は有効な profile の container しか
# 消さない**ので、付けないと seed / runner 系が残る。存在しない profile 名を
# 渡してもエラーにはならないので、ファイルごとに出し分けず全部並べる。
E2E_PROFILES = --profile test --profile bench --profile outbound --profile inbound --profile report

e2e-down-all: ## 検証用スタックを一括撤去 (本番 project mk は対象外)
	@for f in $(E2E_COMPOSE_FILES); do \
		printf "==> %s\n" "$$f"; \
		docker compose -f "$$f" $(E2E_PROFILES) down -v --remove-orphans 2>&1 | tail -1 || true; \
	done

##@ 更新 (運用)

# frontend/ の中でビルドが書き換える tracked ファイル。`pnpm -r build` の i18n
# パッケージが locale.ts を上書きするので、一度でもビルドしたワークツリーは dirty に
# なる。**dirty なまま上流がこのファイルを変えると `git pull` は止まる** — つまり
# frontend の再ビルドが要る回に限って止まる (#2885 で submodule について判明した
# のと同じ型。#3379 で frontend/ を本体へ取り込んだので、本体の側で同じことが起きる)。
# pluginbuild の生成物 (server-plugins.generated.ts) は #3379 で追跡をやめた。
#
# **生成物だけ**戻す。それ以外の変更が残っていれば git 自身が止めるので、
# frontend に手を入れている最中の作業を黙って捨てることはない。
FRONTEND_GENERATED = \
	frontend/packages/i18n/src/autogen/locale.ts

update: ## pull し、frontend 再ビルドの要否を知らせる
	@git checkout -- $(FRONTEND_GENERATED) 2>/dev/null || true
	@before=$$(git rev-parse HEAD:frontend 2>/dev/null); \
	if ! git pull; then \
		printf "\033[31m==> pull に失敗した\033[0m\n"; \
		printf "    frontend/ に手を入れている場合は、変更を commit してからやり直すこと。\n"; \
		exit 1; \
	fi; \
	after=$$(git rev-parse HEAD:frontend 2>/dev/null); \
	if [ "$$before" != "$$after" ]; then \
		printf "\n\033[33m==> frontend/ が更新された。frontend の再ビルドが必要\033[0m\n"; \
		printf "    make docker-update   (Docker Compose 構成)\n"; \
		printf "    make uds-update      (UDS 本番構成)\n"; \
	else \
		printf "\n==> frontend/ に変更なし。frontend の再ビルドは不要\n"; \
	fi

# plugins/*/ のうち独立した git リポジトリのものを更新する。同梱プラグイン
# (status / trustlevel) は mk 本体に tracked なので本体の pull で追従する。
#
# dirty なリポジトリは触らない。勝手に stash すると編集中の変更が「消えた」
# ように見えるうえ、復元手順もどこにも残らない。
pull-plugins: ## plugins/ 配下の独立リポジトリを pull
	@found=0; failed=""; \
	for d in plugins/*/; do \
		[ -e "$$d.git" ] || continue; \
		found=$$((found + 1)); \
		name=$$(basename "$$d"); \
		if [ -n "$$(git -C "$$d" status --porcelain)" ]; then \
			printf "\033[33m==> %-12s skip (未コミットの変更あり)\033[0m\n" "$$name"; \
			continue; \
		fi; \
		if ! git -C "$$d" rev-parse --abbrev-ref '@{u}' >/dev/null 2>&1; then \
			printf "\033[33m==> %-12s skip (upstream 未設定)\033[0m\n" "$$name"; \
			continue; \
		fi; \
		printf "==> %s\n" "$$name"; \
		git -C "$$d" pull --ff-only || failed="$$failed $$name"; \
	done; \
	if [ "$$found" -eq 0 ]; then printf "==> plugins/ に git リポジトリが無い\n"; fi; \
	if [ -n "$$failed" ]; then \
		printf "\033[31m==> pull に失敗:%s\033[0m\n" "$$failed"; \
		exit 1; \
	fi

pull: ## 本体とプラグインをまとめて pull
	$(MAKE) update
	$(MAKE) pull-plugins

# frontend の再ビルドと再起動は必ずセットで行う。mk-go は entry point を
# 起動時に 1 回だけ解決してキャッシュするため、ビルドだけして再起動しないと
# HTML が消えた古い scripts/<hash>.js を指したまま 404 になる。
#
# **`up -d` では足りない。** compose は image と設定が変わらなければコンテナを
# 作り直さないが、frontend は bind mount なので frontend だけ更新したときは
# 何も変わらず、再起動されない (#2885。2026-09-07 に本番で実際に踏んだ)。
# restart を明示したうえで、配信中の entry が実在するかまで見る。
docker-rebuild: ## frontend と image をまとめてビルド (Docker Compose 構成)
	$(MAKE) e2e-frontend-build
	docker compose build

# **本番 UDS を動かしているホストで叩かないこと。** docker-compose.yml は
# `name:` を持たないので project 名がディレクトリ名 `mk` になり、UDS 本番と
# 同じ project へ合流する。app / db / redis が本番の隣に立ち上がり、本番の
# コンテナは orphan 扱いになる (compose 自身が --remove-orphans を勧めてくる)。
# 検証先も .config/docker.yml の url なので、本番ではなく新しく立てた方を見て
# 緑を返す。本番の更新は uds-update を使うこと。
docker-restart: ## app を再起動して配信アセットを検証 (Docker Compose 構成。本番 UDS ホストでは使わない)
	@before=$$(docker compose ps -q app 2>/dev/null); \
	docker compose up -d || exit 1; \
	after=$$(docker compose ps -q app 2>/dev/null); \
	if [ -n "$$before" ] && [ "$$before" = "$$after" ]; then \
		docker compose restart app || exit 1; \
	else \
		printf "==> up -d が起動し直したので restart は省略\n"; \
	fi
	@./deploy/check-frontend-entry.sh $(ENTRY_CHECK_DOCKER_CONFIG)

docker-update: ## pull → ビルド → 再起動 → 検証 (Docker Compose 構成)
	$(MAKE) pull
	$(MAKE) docker-rebuild
	$(MAKE) docker-restart

uds-update: ## pull → ビルド → 再起動 → 検証 (UDS 本番構成)
	$(MAKE) pull
	$(MAKE) uds-rebuild
	$(MAKE) uds-restart


# Binary output
BINARY=elythia
BUILD_DIR=./built

# Go parameters
GOFLAGS=-trimpath

# バージョン情報。MisskeyVersion は /api/meta の version フィールド
# (Misskey TS 互換クライアント向け) で使われる。
#
# 既定では -X を付けず `internal/config` の定数をそのまま使う。以前はここに
# 版数をハードコードしていたが、追従のたびに更新が漏れて `make build` が
# 2 リリース前の版数を報告する状態になっていた (Dockerfile は -X を付けない
# ので本番は無事だったが、`make build` 産のバイナリと route dump が古い版数を
# 名乗っていた)。定数を唯一の source of truth にして二重管理をやめる。
#
# リリースビルドで上書きしたい場合のみ変数を渡す:
#   make build MKGO_VERSION=1.1.1
MKGO_VERSION ?=
MISSKEY_VERSION ?=
LDFLAGS=-s -w
ifneq ($(MKGO_VERSION),)
LDFLAGS += -X github.com/elythia-network/elythia/internal/config.MkGoVersion=$(MKGO_VERSION)
endif
ifneq ($(MISSKEY_VERSION),)
LDFLAGS += -X github.com/elythia-network/elythia/internal/config.MisskeyVersion=$(MISSKEY_VERSION)
endif

# ビルドした revision。/about-mkgo が「mk-go 1.3.0 (abc1234)」として出す (#2700)。
# 同梱 frontend の版 (MkGoFrontendVersion) は、frontend を本体へ取り込んで版が
# 本体と同じになったので廃止した (#3379)。
#
# **`$(shell ...)` は使わない。** make の parse 時に必ず走るので、target と
# 無関係な `make help` でも git を呼ぶことになるうえ、`gaterun-check` が
# 「この Makefile に `$(shell …)` が無いので `make -pn` に副作用が無い」という
# 前提で回っている (docs/gates.md の 2026-09-06 の経緯、gaterun-check)。recipe 内の `$$(...)` なら展開は
# 実行時だけで、`make -n` では表示されるだけになる。
#
# git が無い / リポジトリ外でビルドした場合は空のまま。読む側が「不明」として
# 扱うので、ここで `unknown` のような値を作らない (表示に出てしまう)。
REVISION_LDFLAGS = -X github.com/elythia-network/elythia/internal/config.MkGoCommit=$$(git rev-parse --short HEAD 2>/dev/null)

##@ 開発
plugins: ## plugins/ を走査して組み込み用ファイルを生成 (#2480)
	GOWORK=off go run ./tools/pluginbuild

# CI の build job と frontend workflow が使う。同梱サンプルは mk-plugin.yml で既定無効なので、
# 既定の走査では検証対象から外れてしまう (#2495)。
plugins-all: ## disabled のプラグインも含めて生成 (CI 検証用)
	GOWORK=off go run ./tools/pluginbuild -include-disabled

# プラグイン開発用。ソースを監視して 生成 → ビルド → 再起動 を繰り返す (#2477)。
# frontend の HMR は別端末の Vite dev server が担う:
#   cd frontend/packages/frontend && pnpm watch
# GOWORK=off は plugindev 自体を stale な go.work から守るために要る (消した
# プラグインを指したままだと go run が起動すらしない)。内側の
# go build ./cmd/elythia は plugindev が GOWORK= で明示的に戻すので、
# ここで off にしても生成物が import するプラグインのモジュールは go.work 経由で解決できる。
plugin-dev: ## プラグインを編集しながら動かす (PLUGIN=plugins/status)
	GOWORK=off go run ./tools/plugindev $(if $(PLUGIN),-plugin $(PLUGIN),)

build: plugins ## バイナリを ./built/elythia に生成
	go build $(GOFLAGS) -ldflags "$(LDFLAGS) $(REVISION_LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/elythia

run: build ## build して起動
	$(BUILD_DIR)/$(BINARY) serve -config .config/default.yml

# **ビルド済みフロントが無いときだけ MK_DEV=1 を立てる。** mk-go は dev モード
# (`dev: true` / MK_DEV=1) でしか `/vite/*` を dev server へ流さない —
# 本番でビルド出力が欠けたときに、認証なしで localhost:5173 へ reverse proxy
# されていたため。以前の `make dev` は「無ければ proxy」の暗黙の挙動に頼って
# いたので、同じ条件をここで明示する。ビルド済みなら従来どおりそれを配る。
# 呼び出し側が MK_DEV を export していればそちらを優先する。
# (recipe の中に置くと make -n / 実行時にこのコメントが echo されるので外に置く)
dev: ## go run で直接起動 (ビルド済みフロントが無ければ Vite dev server を使う)
	@if [ -z "$${MK_DEV+x}" ] && [ ! -d "$${MISSKEY_FRONTEND_DIR:-frontend/built/_frontend_vite_}" ]; then \
		echo "make dev: ビルド済みフロントが無いので MK_DEV=1 で起動します (Vite dev server を localhost:5173 で立てること)"; \
		export MK_DEV=1; \
	fi; \
	go run ./cmd/elythia serve -config .config/default.yml

clean: ## ビルド成果物を削除
	rm -rf $(BUILD_DIR)

tidy: ## go mod tidy
	go mod tidy

test: ## 全テストを実行 (CI と同じ -race / -count=1 / -shuffle seed)
	# CI (.github/workflows/ci.yml) の test-shards と条件を揃える (#2841)。
	# -shuffle の seed を揃えたのは #2795。順序依存は seed 固定で塞いだのに
	# **データ競合は塞げていなかった** ので、同じ理屈で -race も揃える
	# (実測 65s -> 160s。CI は push から結果まで 4-5 分かかるので往復するより速い)。
	# -count=1 は CI との一致のため。-shuffle は cacheable flag ではないので、
	# seed を渡している時点でキャッシュは元から効いていない。
	# -race は cgo を要求する (CGO_ENABLED=0 の環境では make test-fast を使う)。
	# -timeout / -coverprofile / -covermode は揃えなくてよい。前者は既定と同じ
	# 10m で、後 2 つはカバレッジ閾値チェック用なので挙動に影響しない。
	go test ./... -v -race -count=1 -shuffle=3

.PHONY: test-fast
test-fast: ## 全テストを -race 抜きで実行 (反復用。コミット前は make check を使う)
	# **これはコミット前の検査ではない。** -race が無いので CI で落ちるものが
	# 手元で緑になる。編集しながら回す用 (実測で make test の 1/2.5)。
	# -count=1 は落とさない — -shuffle を外したときに (cached) で無検証の緑を
	# 返すようになるため。
	go test ./... -count=1 -shuffle=3

.PHONY: emoji-regex emoji-regex-check
emoji-regex: ## MFM の Unicode 絵文字の正規表現を mfm-js の emoji-data から生成 (#3324)
	# 生成物 (internal/activitypub/mfm/emoji_regex_gen.go) を手で直さない。
	# frontend/ に pnpm install 済みであること。mfm-js か emoji-data の版が
	# 変わったら、mfm-js の期待値 (internal/activitypub/mfm/testdata/emoji_mfmjs.json) も
	# testdata/emoji_mfmjs.mjs で作り直す (どちらかの版がずれるとテストが落ちる)。
	GOWORK=off go run ./tools/emojiregex

emoji-regex-check: ## 生成した絵文字の正規表現が frontend/ の mfm-js / emoji-data と一致するか検査
	# **`make gates` には入れない** — node_modules が要る。frontend-check から呼ぶ。
	# 生成物と snapshot の突き合わせは node_modules 無しで `go test ./tools/emojiregex/`
	# が見る。こちらは snapshot (正規表現と、mfm-js / emoji-data の版) が frontend/ に
	# pnpm install したものと一致しているかを見る。
	GOWORK=off go run ./tools/emojiregex -check

plugin-doc-check: ## authoring.md の Go スニペットがコンパイルできるか検査
	./tests/plugin-doc/check-snippets.sh

plugin-vet: ## 同梱プラグインの既定無効を検査 + go vet (CI の build job の 2 step 相当)
	@set -e; \
	markers=$$(git ls-files 'plugins/*/mk-plugin.yml'); \
	if [ -z "$$markers" ]; then echo "同梱プラグインの mk-plugin.yml が見つかりません (列挙が壊れています)"; exit 1; fi; \
	fail=0; \
	for f in $$markers; do \
		if grep -qE '^disabled:[[:space:]]*true[[:space:]]*$$' "$$f"; then echo "ok   $$f"; \
		else echo "FAIL $$f — 'disabled: true' の行 (完全一致) がありません。disabled を含む行:"; \
			grep -n disabled "$$f" || echo "  (無し)"; fail=1; fi; \
	done; \
	[ "$$fail" -eq 0 ]; \
	mods=$$(git ls-files 'plugins/*/go.mod'); \
	if [ -z "$$mods" ]; then echo "同梱プラグインが見つかりません (列挙が壊れています)"; exit 1; fi; \
	for mod in $$mods; do \
		dir=$$(dirname "$$mod"); \
		echo "==> $$dir"; \
		(cd "$$dir" && GOWORK=off go vet ./...); \
	done

plugin-test: ## 同梱プラグインのテストを実行 (PostgreSQL が要る)
	@set -e; \
	mods=$$(git ls-files 'plugins/*/go.mod'); \
	if [ -z "$$mods" ]; then echo "同梱プラグインが見つかりません"; exit 1; fi; \
	for mod in $$mods; do \
		dir=$$(dirname "$$mod"); \
		echo "==> $$dir"; \
		(cd "$$dir" && MK_PLUGIN_TESTS_REQUIRE_DB=1 GOWORK=off go test ./... -count=1); \
	done

fmt: ## gofmt -s -w . で整形
	# **PATH の gofmt ではなく go.mod の toolchain のものを使う。** gofmt は版で
	# 整形結果が変わる (Go 1.27 でコメントの桁揃えが変わった) が、PATH の gofmt は
	# GOTOOLCHAIN の切り替えに追従しない。手元で緑なのに CI (setup-go が go.mod の
	# 版を入れる) の Format check で落ちる (実測)。
	"$$(go env GOROOT)/bin/gofmt" -s -w .

lint: ## go vet ./...
	go vet ./...

.PHONY: golangci-lint
golangci-lint: ## golangci-lint (errcheck / govet / ineffassign / staticcheck)
	# `go vet` だけでは見えない層を埋める。設定は .golangci.yml。
	#
	# **版を固定する。** 新しいチェックが増えると、コードを触っていない PR が
	# 赤くなる。`lint` は required check なので、上げるのは明示的な操作にする。
	# CI もこの target を呼ぶので、版の定義はここ 1 箇所だけ。
	#
	# **CI と同じ条件で回すために `GOWORK=off` を付ける。**
	# `make build` を一度でも回すと `go.work` と `cmd/elythia/plugins_generated.go`
	# が出来る。CI は clean checkout でどちらも持たないので、揃えないと手元だけ
	# 結果が変わる (actionlint の shellcheck で踏んだのと同じ型)。
	#
	# **`go.work` が変えるのは解析対象ではなく build list。** `./...` は module
	# 境界を越えないので `plugins/` は lint されない (実測で package 数 207 が
	# go.work の有無で一致)。変わるのは**依存の選択版**で、workspace 内の module の
	# require が MVS に参加するぶん共通依存が上がる (実測: `golang.org/x/telemetry`
	# が 2025-10-08 → 2026-07-08)。
	#
	# **生成物のほうは退避が要る。** `plugins_generated.go` は private な plugins を
	# import するので `GOWORK=off` では解決できず typecheck で落ちる。**typecheck が
	# 落ちると golangci-lint は他の解析結果を報告しない**ので、別パッケージの違反が
	# 黙って見えなくなる (実測)。`make plugins` で作り直せるので退避して戻す
	# (trap 付きなので中断しても復元される)。**`cp -p` で mode も保存する** —
	# mktemp は 0600 で作るので、素の `cp` だと復元後に 644 → 600 になる (実測)。
	#
	# **toolchain を go.mod の版に固定する。** `go run pkg@version` はこの
	# リポジトリの go.mod を見ず、golangci-lint 自身の `go` directive (1.26.0) を
	# 基準に toolchain を選ぶ。手元の go が go.mod より古いとそれでビルドされ、
	# `the Go language version (go1.26) used to build golangci-lint is lower than
	# the targeted Go version (1.27.1)` で止まる (Go 1.27.1 への更新で実測)。
	@set -e; \
	gen=cmd/elythia/plugins_generated.go; bak=""; \
	if [ -f "$$gen" ]; then bak=$$(mktemp); cp -p "$$gen" "$$bak"; rm -f "$$gen"; fi; \
	trap 'if [ -n "$$bak" ]; then cp -p "$$bak" "$$gen"; rm -f "$$bak"; fi' EXIT INT TERM; \
	gover=$$(awk '/^go [0-9]/ {print $$2; exit}' go.mod); \
	GOWORK=off GOTOOLCHAIN=go$$gover go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run --timeout 10m

.PHONY: actionlint
actionlint: ## GitHub Actions の workflow を検査
	# CodeQL の `actions` クエリが見るのは script injection などの**セキュリティ**で、
	# 式の typo・存在しない needs 参照・`runs-on` の誤りといった**正しさ**は見ない。
	# workflow のミスは動かすまで分からないので (#2940 で実際に踏んだ)、静的に落とす。
	# `run:` の中身は actionlint が shellcheck へ渡す。
	#
	# **バージョンを固定する。** 新しい検査が増えると、workflow を触っていない PR が
	# 赤くなる。`lint` は required check なので、上げるのは明示的な操作にする。
	#
	# **shellcheck が無いと黙って検査が減る。** actionlint は `run:` の中身を
	# shellcheck へ渡すが、無ければその分だけ落として成功で返す。CI の
	# ubuntu-latest には入っているので、**手元だけ通って CI で落ちる**
	# (実測: 手元 0 件 / CI 10 件。うち 1 件は二重引用符の中のバッククォートが
	# コマンド置換として実行される実バグだった)。skip を成功として扱わない。
	@command -v shellcheck >/dev/null 2>&1 || { \
		echo "shellcheck が見つかりません。" >&2; \
		echo "actionlint は run: の中身をこれに渡すので、無いまま実行すると CI より弱い検査になります。" >&2; \
		echo "  Debian/Ubuntu: sudo apt install shellcheck" >&2; \
		echo "  承知で飛ばす:   MK_ACTIONLINT_ALLOW_NO_SHELLCHECK=1 make actionlint" >&2; \
		[ -n "$$MK_ACTIONLINT_ALLOW_NO_SHELLCHECK" ]; }
	go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12

# Migration
#
# 接続先は -config (既定 .config/default.yml) から決まる。DATABASE_URL は読まない。
# 別の DB へ流すなら -config を渡すか MK_DB_* で上書きする。
##@ マイグレーション
migrate-up: ## マイグレーションを最新まで適用
	go run ./cmd/elythia migrate -direction up

# **-steps 1 は必須。** `elythia migrate` は steps 未指定 (0) を「全部」と解釈するので、
# 付け忘れると 1 段のつもりで全 down が走り 全テーブルが消える。
# 適用済みが 0 件のときは golang-migrate が "file does not exist" で exit 1 する
# (steps 指定時は ErrNoChange に落ちないため)。冪等に叩くなら呼び出し側で吸収する。
migrate-down: ## マイグレーションを 1 段階ロールバック
	go run ./cmd/elythia migrate -direction down -steps 1

migrate-create: ## 新規マイグレーションファイルを作成
	@read -p "Migration name: " name; \
	touch migration/$$(printf "%06d" $$(($$(ls migration/*.up.sql 2>/dev/null | wc -l) + 1)))_$${name}.up.sql; \
	touch migration/$$(printf "%06d" $$(($$(ls migration/*.down.sql 2>/dev/null | wc -l) + 1)))_$${name}.down.sql

# Docker
##@ Docker

# 配信アセットの検証で url を読む先。operator が独自設定を置いていればそちら、
# 無ければ image に焼き込まれる .example を見る (Dockerfile と同じ既定)。
#
# **`DOCKER_CONFIG` という名前にしてはいけない。** docker CLI が設定
# ディレクトリとして読む予約名で、make は環境由来の変数を recipe へ export し
# 直すため、operator の環境にそれがあると値を奪って `docker compose` 自体が
# `unknown command` で死ぬ (このリポジトリの docker 系 target が全滅する)。
ENTRY_CHECK_DOCKER_CONFIG=$(if $(wildcard .config/docker.yml),.config/docker.yml,.config/docker.yml.example)

docker-build: ## Docker イメージをビルド
	docker build -t mk-go .

docker-up: ## docker compose up -d
	docker compose up -d

docker-down: ## docker compose down
	docker compose down

##@ pull して起動 (ビルド不要)

# publish 済みの bundled image を pull して動かす。フロントエンドのビルドも
# image のビルドも不要。既存の docker-compose.yml / make docker-* はそのまま
# 使えるので、こちらは置き換えではなく並立する選択肢。
IMAGE_COMPOSE=docker-compose.image.yml

image-up: ## bundled image を pull して起動
	docker compose -f $(IMAGE_COMPOSE) up -d

image-down: ## 上記スタックを撤去
	docker compose -f $(IMAGE_COMPOSE) down

image-down-v: ## 上記スタックを volume ごと撤去
	docker compose -f $(IMAGE_COMPOSE) down -v

image-logs: ## 上記スタックのログを表示
	docker compose -f $(IMAGE_COMPOSE) logs -f

image-build: ## bundled image を手元でビルドする (publish 前の確認用)
	docker build -f Dockerfile.bundled -t ghcr.io/shiroha-a/mk:bundled .


# Federation tests ― 本家 Misskey と実際に立ち上げて連合動作を検証する。
# 各ターゲット (misskey / mastodon / pleroma / ...) ごとに tests/federation/compose.<target>.yml を用意する。
FEDERATION_MISSKEY_COMPOSE=tests/federation/compose.misskey.yml

##@ e2e: 連合
federation-misskey-build: ## 連合テスト用 Misskey イメージをビルド
	docker compose -f $(FEDERATION_MISSKEY_COMPOSE) build

federation-misskey-up: ## 連合テスト用 Misskey インスタンスを起動
	docker compose -f $(FEDERATION_MISSKEY_COMPOSE) up -d --build

federation-misskey-test: ## 連合テストを実行
	docker compose -f $(FEDERATION_MISSKEY_COMPOSE) --profile test run --rm test-runner

# 起動 → healthy 待ち → pytest → 撤去 を 1 コマンドで通す。CI から呼ぶのはこれ。
# 個別の up / test を手で叩くのと違い、失敗しても trap で必ず後始末する。
federation-misskey-e2e: ## 連合テストを起動から撤去まで通しで実行
	./tests/federation/run-misskey-test.sh

federation-misskey-down: ## 連合テストスタックを撤去
	docker compose -f $(FEDERATION_MISSKEY_COMPOSE) --profile test down -v

federation-misskey-logs: ## 連合テストスタックのログを表示
	docker compose -f $(FEDERATION_MISSKEY_COMPOSE) logs -f

# 本物の Mastodon を相手にした実連合 e2e (#3234)。引用の承認 (FEP-044f) を見る。
FEDERATION_MASTODON_COMPOSE=tests/federation/compose.mastodon.yml

federation-mastodon-e2e: ## Mastodon との連合テストを起動から撤去まで通しで実行
	./tests/federation/run-mastodon-test.sh

federation-mastodon-down: ## Mastodon との連合テストスタックを撤去
	docker compose -f $(FEDERATION_MASTODON_COMPOSE) --profile test down -v

# Drop-in e2e (#365) ― Misskey TS 2 インスタンス (A, B) を立ち上げて
# 連合基盤を検証する。Phase 13-1 では TS ↔ TS の smoke test のみ。
# Phase 13-2 以降で mk 差し替え overlay を追加する予定。
DROPIN_COMPOSE=tests/dropin/compose.yml

##@ e2e: drop-in 互換
dropin-up: ## drop-in e2e スタック (TS 2 インスタンス) を起動
	docker compose -f $(DROPIN_COMPOSE) up -d --build

dropin-test: ## drop-in e2e の smoke test を実行
	docker compose -f $(DROPIN_COMPOSE) --profile test run --rm test-runner

dropin-down: ## drop-in e2e スタックを撤去
	docker compose -f $(DROPIN_COMPOSE) --profile test down -v

dropin-logs: ## drop-in e2e スタックのログを表示
	docker compose -f $(DROPIN_COMPOSE) logs -f

# Drop-in mk overlay (#367) — instance A の backend を mk-go に差し替えた
# 状態で TS-A 用 stack を起動する。連合先 (instance B) は TS のままなので
# mk ↔ TS federation も同時に検証できる。
DROPIN_MK_OVERLAY=tests/dropin/compose.mk.yml

dropin-mk-up: ## drop-in e2e に mk-go overlay を適用して起動
	docker compose -f $(DROPIN_COMPOSE) -f $(DROPIN_MK_OVERLAY) up -d --build

dropin-mk-test: ## mk-go overlay に対する smoke test を実行
	docker compose -f $(DROPIN_COMPOSE) -f $(DROPIN_MK_OVERLAY) --profile test run --rm test-runner

dropin-mk-down: ## mk-go overlay を撤去
	docker compose -f $(DROPIN_COMPOSE) -f $(DROPIN_MK_OVERLAY) --profile test down -v

dropin-mk-logs: ## mk-go overlay のログを表示
	docker compose -f $(DROPIN_COMPOSE) -f $(DROPIN_MK_OVERLAY) logs -f

# Drop-in swap シナリオ (#367): TS-A → mk-A 切替で state が引き継げることを
# 検証する end-to-end テスト。bash orchestrator が以下を順次実行する:
#   1. TS-A + TS-B 起動
#   2. test_swap_setup.py で alice/bob/follow/note を作る
#   3. TS-A backend を停止
#   4. overlay で mk-A 起動 (DB-A / Redis-A はそのまま)
#   5. test_swap_verify.py で state preserved + 新規 federation を確認
dropin-swap-test: ## TS → mk-go 切替の state preservation を通しで検証
	./tests/dropin/run-swap-test.sh

# Drop-in fedibird-mock e2e (#1083) — base + mk + fedibird overlay の stack で
# Fedibird-like ActivityPub mock を立てて、mk-A との Ed25519 双方向 verify を
# walks through する。ed25519 P2-P5 が実 federation 経路で動くことを担保する
# nightly 用 e2e。
# mk-go 生まれの DB を TS に引き渡す経路 (#2379)。swap test (TS→mk-go→TS) とは
# 別物で、TS が一度も触っていない schema を受け取る。mk-go で始めた人が Misskey に
# どこまで移れるかを測る (保証はしない、#3191)。
dropin-mkgo-born-test: ## mk-go 生まれの DB を TS に引き渡せるか検証
	./tests/dropin/run-mkgo-born-test.sh

dropin-fedibird-test: ## Fedibird-like AP mock との Ed25519 双方向 verify
	./tests/dropin/run-fedibird-test.sh

# Drop-in frontend e2e (#380 / Phase 14) ― 3 Misskey TS インスタンス上で
# cypress を回して、共有 TS フロントエンドから観測可能なアクティビティの
# 整合性を検証する基盤。Phase 14-1 は baseline (all TS) のみ。
DROPIN_FRONTEND_COMPOSE=tests/dropin-frontend/compose.yml

dropin-frontend-up: ## drop-in frontend e2e スタックを起動
	docker compose -f $(DROPIN_FRONTEND_COMPOSE) up -d

dropin-frontend-down: ## drop-in frontend e2e スタックを撤去
	docker compose -f $(DROPIN_FRONTEND_COMPOSE) --profile test down -v

dropin-frontend-logs: ## drop-in frontend e2e のログを表示
	docker compose -f $(DROPIN_FRONTEND_COMPOSE) logs -f

# baseline: all TS な状態で cypress spec が全 pass することを確認する
# (Phase 14-1 #381)。
dropin-frontend-baseline: ## 3 TS インスタンス + cypress で baseline spec を実行
	./tests/dropin-frontend/run-frontend-baseline.sh

# Phase 14-3 (#394): TS-A → mk-A 切替後も cypress spec が引き続き pass する
# ことを確認する swap test orchestrator。baseline 実行 → TS-A 停止 → mk-A
# 起動 → swap モードで cypress 再実行、を bash で順次制御する。
DROPIN_FRONTEND_MK_OVERLAY=tests/dropin-frontend/compose.mk.yml

# mk-go overlay を直接立ち上げる (手動デバッグ用)。DB は clean からだが、
# Phase 14-3 の本 test は `dropin-frontend-swap-test` を使う。
dropin-frontend-mk-up: ## drop-in frontend e2e に mk overlay を適用して起動
	docker compose -f $(DROPIN_FRONTEND_COMPOSE) -f $(DROPIN_FRONTEND_MK_OVERLAY) up -d --build

dropin-frontend-mk-down: ## drop-in frontend e2e の mk overlay を撤去
	docker compose -f $(DROPIN_FRONTEND_COMPOSE) -f $(DROPIN_FRONTEND_MK_OVERLAY) --profile test down -v

dropin-frontend-swap-test: ## TS-A → mk-A 切替まで含む frontend e2e
	./tests/dropin-frontend/run-frontend-swap-test.sh

# 本家フロントエンドの取得とビルド。
#
# フロントエンドは本体の frontend/ (#3379 で fork から取り込んだ pnpm workspace) から
# ビルドする。比較対象の本家は .cache/misskey/<版> から読む (make upstream-fetch、#3378)。
#
# `e2e-frontend-build` は pnpm を docker run で実行する。ビルドに使う Node の版と
# distro を、upstream がコンテナでビルドするときの組み合わせにそろえるため
# (下の #2921 の段落)。Makefile の pnpm がすべてそうなっているわけではない —
# `upstream-e2e-deps` はホストの pnpm を使う。
#
# frontend e2e は Playwright に一本化した (#2437)。Cypress ラッパーは本家が
# Cypress を廃止して参照先が消滅したため削除済み。spec は tests/playwright/。
# **Node の版は frontend/.node-version から、distro は FRONTEND_NODE_DISTRO から取る。**
# 以前は upstream の Dockerfile の `ARG NODE_VERSION` (`26.4.0-trixie` の形で、版と
# distro の両方を持つ) から取っていた (#2921) が、#3379 で本家の Dockerfile は
# 取り込まなくなった。版は CI (`node-version-file`) と同じ .node-version を見て、
# distro は upstream 2026.10.0 の Dockerfile と同じ trixie を置く。本家の版を上げた
# ときは、本家の Dockerfile の distro が変わっていないかを確かめる。
# recipe の中で読む — `$(shell ...)` は使わない (REVISION_LDFLAGS の理由)。
#
# 以前は `node:22-bookworm` 固定で、CI が `.node-version` (Node 26) を使うのに
# **本番のビルドだけ Node 22** という食い違いがあった。しかも `packages/backend` の
# `engines.node` は `^22.22.2 || ...` で、node:22-bookworm の 22.22.2 は**下限
# ちょうど**。upstream が下限を上げた瞬間に本番のビルドだけが engines で弾かれ、
# CI は緑のまま気付けない。
E2E_WORKDIR=/work
FRONTEND_NODE_DISTRO ?= trixie

##@ frontend build
# frontend/ を Docker 内でビルドする。数分〜10 分程度かかる。
# 成果物は frontend/built と frontend/packages/*/built に出力される。
#
# CI=true を渡す理由: upstream 2026.5.2 で pnpm 10 → 11 に移行 (#17400 dep bump
# 系)、pnpm 11 は previous install (node_modules) を消す前に prompt を出す挙動が
# default。docker run は TTY 無し起動なので prompt が出せず ERR_PNPM_ABORTED_
# REMOVE_MODULES_DIR_NO_TTY で abort する。CI=true で skip させる。
# plugins を先に走らせる。生成物が無いとプラグインの frontend が取り込まれず、
# backend にだけ入った片肺の状態になる (#2479)。
# **corepack は使わない (#2921)。** Node.js 26 の配布物に corepack は含まれて
# いない (`node:26-bookworm` で `command not found` を実測)。`.node-version` に
# 追従して image を上げると同時に踏むので、`npm i -g pnpm@<packageManager>` に
# 変えてある。**版は frontend/package.json の `packageManager` から取る** — CI は #2914 で
# `pnpm/action-setup` + `package_json_file` に寄せてあり、これで両者が同じ
# 定義を見る。
#
# **拾えなかったら落とす。** docker 側は空タグ (`node:`) なら
# `invalid reference format` で落ちるので実は安全側だが、**pnpm 側は
# `npm i -g pnpm@` が exit 0 で最新を入れてしまう** (実測)。ガードが効いている
# のは pnpm 側で、黙って別の版でビルドするのを止めている。
# **node_modules の ABI に注意。** このターゲットはコンテナの Node で
# `node_modules` を作り直すが、同じ木を**ホストで動く target** も使う
# (`frontend-check` / `frontend-lint` / `frontend-test` / `upstream-e2e-test`)。
# ホストの Node が違う版だと、ABI 固定の native module (`re2`) が
# NODE_MODULE_VERSION 不一致で落ちる。**ホストも `.node-version` に揃えるのが前提**
# (devcontainer は postCreate.sh がそうする)。ずれた場合はホスト側で
# `pnpm install` を流し直せば直る。
e2e-frontend-build: plugins ## フロントエンドをビルド (本番の bind-mount 先を上書きするので注意)
	@node_ver=$$(tr -d '[:space:]' < frontend/.node-version); \
	node_tag="$$node_ver-$(FRONTEND_NODE_DISTRO)"; \
	pnpm_ver=$$(sed -n 's/.*"packageManager"[[:space:]]*:[[:space:]]*"pnpm@\([^"+]*\).*/\1/p' \
		frontend/package.json | head -1); \
	if [ -z "$$node_ver" ]; then echo "frontend/.node-version を読めない" >&2; exit 1; fi; \
	if [ -z "$$pnpm_ver" ]; then echo "package.json の packageManager を読めない" >&2; exit 1; fi; \
	echo "==> node:$$node_tag / pnpm@$$pnpm_ver でビルドする"; \
	docker run --rm -e CI=true -v $(PWD):$(E2E_WORKDIR) -w $(E2E_WORKDIR)/frontend \
		"node:$$node_tag" \
		bash -lc "npm i -g pnpm@$$pnpm_ver && pnpm install --frozen-lockfile && pnpm build"

# UDS-only compose stack (Phase 12-2)。Phase 12-1 で入った UNIX domain socket
# 対応を使って nginx → mk-go → postgres / valkey をすべて UDS で繋ぐ。
# 詳細は docs/docker-uds.md を参照。
UDS_COMPOSE=compose.uds.yaml
UDS_CONFIG=deploy/uds/config/default.yml

# compose / config の実ファイルはデプロイ先ごとに書き換えるため gitignore 済み。
# 初回起動時のみ .example からコピーする (order-only prerequisite なので、
# 一度作成したあとは .example を更新してもユーザのローカル編集を上書きしない)。
$(UDS_COMPOSE):
	cp $(UDS_COMPOSE).example $(UDS_COMPOSE)

$(UDS_CONFIG):
	cp $(UDS_CONFIG).example $(UDS_CONFIG)

##@ 本番 UDS (実行注意)
uds-init: | $(UDS_COMPOSE) $(UDS_CONFIG) ## UDS 構成を初期化

# frontend/ を docker 内でビルドする。初回は 3〜10 分程度かかる。
# 既存 e2e-frontend-build のエイリアス (成果物先が同じなので共有して OK)。
# **出力先は frontend/built** (#3379 から。以前は third_party/misskey/built)。本番の
# compose が bind mount する元も合わせて変える (docs/deployment.md の切り替え手順)。
# 検査を先に終わらせてからビルドする (前提に並べると -j で並行して走る)。
uds-frontend-build: uds-layout-check ## 本番向けフロントエンドをビルド (本番の配信物を差し替える)
	$(MAKE) e2e-frontend-build

# #3379 より前の compose.uds.yaml (gitignore 済み) は third_party/misskey の
# built と assets を bind mount している。submodule を外した後も作業ツリーには古い
# third_party/ が残りうる (git は未追跡になったディレクトリを消さない) ので、
# そのまま uds-update すると新しい frontend/built を作っても誰も mount せず、
# 古い SPA を警告無しで配り続ける (entry の検証も通ってしまう)。切り替え手順を
# 踏むまで止める。
uds-layout-check:
	@if [ -f $(UDS_COMPOSE) ] && grep -nE '^[^#]*third_party/misskey([/:"]|$$)' $(UDS_COMPOSE); then \
		echo "==> $(UDS_COMPOSE) がまだ third_party/misskey を mount している。" >&2; \
		echo "    docs/deployment.md の「frontend を本体へ取り込んだ版へ上げる (#3379)」に従って向け直す" >&2; \
		exit 1; \
	fi

# revision は build-arg で渡す。**Dockerfile の中では git を呼べない** —
# `.dockerignore` が `.git` を落とすので、コンテキストにリポジトリが入らない。
uds-build: uds-layout-check | $(UDS_COMPOSE) $(UDS_CONFIG) ## UDS スタックのイメージをビルド
	MKGO_COMMIT=$$(git rev-parse --short HEAD 2>/dev/null) \
	docker compose -f $(UDS_COMPOSE) build

uds-up: uds-layout-check | $(UDS_COMPOSE) $(UDS_CONFIG) ## UDS スタックを起動
	docker compose -f $(UDS_COMPOSE) up -d --build

uds-rebuild: ## frontend と image をまとめてビルド (本番の配信物を差し替える)
	$(MAKE) uds-frontend-build
	$(MAKE) uds-build

# up -d は image と設定が変わらなければコンテナを作り直さない。frontend は
# bind mount なので、frontend だけ更新したときは mkgo が再起動されず、起動時に
# キャッシュした古い entry を配り続ける (実体は新しいビルドで消えているので
# 404、#2885)。restart を明示し、配信中の entry が実在するかまで確かめる。
uds-restart: uds-layout-check | $(UDS_COMPOSE) $(UDS_CONFIG) ## mkgo を再起動して配信アセットを検証
	@before=$$(docker compose -f $(UDS_COMPOSE) ps -q mkgo 2>/dev/null); \
	docker compose -f $(UDS_COMPOSE) up -d || exit 1; \
	after=$$(docker compose -f $(UDS_COMPOSE) ps -q mkgo 2>/dev/null); \
	if [ -n "$$before" ] && [ "$$before" = "$$after" ]; then \
		docker compose -f $(UDS_COMPOSE) restart mkgo || exit 1; \
	else \
		printf "==> up -d が起動し直したので restart は省略\n"; \
	fi
	@./deploy/check-frontend-entry.sh $(UDS_CONFIG)

uds-down: | $(UDS_COMPOSE) ## UDS スタックを停止
	docker compose -f $(UDS_COMPOSE) down

# named volume も含めて完全削除する (DB データも全部消える)。
uds-down-v: | $(UDS_COMPOSE) ## UDS スタックを volume ごと削除
	docker compose -f $(UDS_COMPOSE) down -v

uds-logs: | $(UDS_COMPOSE) ## UDS スタックのログを表示
	docker compose -f $(UDS_COMPOSE) logs -f

uds-ps: | $(UDS_COMPOSE) ## UDS スタックのコンテナ一覧
	docker compose -f $(UDS_COMPOSE) ps

# Benchmark ― mk-go vs 本家 Misskey のストレステスト比較。
# k6 (Docker) で同一エンドポイントに負荷をかけ、レイテンシ・スループットを比較する。
# 結果は tests/bench/http/results/report.md に出力される。
BENCH_COMPOSE=tests/bench/http/compose.yml

##@ ベンチマーク
bench-up: ## k6 ベンチのスタックを起動
	docker compose -f $(BENCH_COMPOSE) up -d --build

bench-run: ## k6 ベンチを実行
	docker compose -f $(BENCH_COMPOSE) --profile bench up --abort-on-container-exit compare

# `--profile bench` が要る。**`down` は有効な profile のコンテナしか消さない**
# ので、付けないと seed / k6 / compare / profile-collector が残る。残った
# container は撤去済み network の ID を掴んだままなので、次の `up` が
# `network <hash> not found` で落ちる (queue-bench が #1163 / #2364 で
# `--force-recreate` を撒いて対症療法していたのと同じ現象。こちらは残さない
# ことで根本を断つ)。
bench-down: ## k6 ベンチのスタックを撤去
	docker compose -f $(BENCH_COMPOSE) --profile bench down -v

bench-logs: ## k6 ベンチのログを表示
	docker compose -f $(BENCH_COMPOSE) logs -f

# Queue bench (#563): deliver/inbox throughput comparison between
# Misskey TS (BullMQ) and mk-go (mkq). asynq driver は #2985 で削除。
QUEUE_BENCH_COMPOSE=tests/bench/queue/compose.yml

queue-bench-up: ## queue-bench スタックを起動
	docker compose -f $(QUEUE_BENCH_COMPOSE) up -d --build

queue-bench-seed: ## queue-bench 用のデータを投入
	# `--force-recreate` で seed container を毎回 fresh に作る (#1163)。
	#
	# `--no-deps` が要る。付けないと --force-recreate が依存 (app-mkq /
	# app-ts) まで作り直し、それらの IP が変わる。nginx の upstream は
	# `server app-mkq:3000;` とホスト名で書かれていて **起動時に一度だけ**
	# 名前解決するため、nginx は死んだ IP を掴んだまま 502 を返し続ける。
	# seed の wait_health は例外にならない 502 を 240 秒受け取って
	# `not ready: None` で落ちる。IP が再利用されるかは運次第なので、
	# nightly が 8 回中 6 回落ちる flaky の正体だった (#2364)。
	# down → up を繰り返すと network が再作成されて新 ID になるが、profile
	# container は queue-bench-down (= `down -v`) の対象外で残る。古い container
	# は attach 先の network ID が変わったまま固定されて、次回 start 時に
	# `network <hash> not found` で失敗する非決定性を引き起こすため、毎回
	# 強制的に再作成する。同じ理由を queue-bench-outbound / -inbound / -report
	# にも適用している。
	docker compose -f $(QUEUE_BENCH_COMPOSE) --profile bench up --abort-on-container-exit --force-recreate --no-deps seed
	# meta cache (5min TTL) が古い federation='none' を握っているので、seed
	# 後に app コンテナを再起動して新しい meta.federation='all' を読ませる。
	docker compose -f $(QUEUE_BENCH_COMPOSE) restart --no-deps app-mkq app-ts
	@echo "waiting for apps to become healthy after restart..."
	@for i in $$(seq 1 60); do \
		MKQ=$$(docker compose -f $(QUEUE_BENCH_COMPOSE) ps app-mkq --format json 2>/dev/null | grep -o '"Health":"healthy"' || true); \
		TS=$$(docker compose -f $(QUEUE_BENCH_COMPOSE) ps app-ts --format json 2>/dev/null | grep -o '"Health":"healthy"' || true); \
		if [ -n "$$MKQ" ] && [ -n "$$TS" ]; then \
			echo "ready (mkq+ts all healthy)"; exit 0; \
		fi; \
		sleep 2; \
	done; \
	echo "warning: not all apps became healthy in time" >&2; exit 1
	# **nginx も再起動する (#2917)。** `restart` はコンテナに IP を割り当て直し
	# うるので、app 同士が IP を交換すると、upstream をホスト名で書いた nginx が
	# **古いアドレスを掴んだまま相手側の app へ繋ぐ**。実測では nginx-ts が
	# app-mkq に、nginx-mkq が app-ts に繋がり、Host が食い違って inbound が
	# 全件 401 になっていた (9/4 から 6 夜連続で nightly が赤かった原因)。
	# #2364 で seed の `--force-recreate` に `--no-deps` を足して依存の作り直しは
	# 止めたが、**その次の restart は塞がっていなかった**。
	#
	# **app が healthy になってから restart する。** nginx は起動時に一度だけ
	# 名前解決するので、app の IP が確定した後でなければ意味が無い。
	docker compose -f $(QUEUE_BENCH_COMPOSE) restart --no-deps nginx-mkq nginx-ts
	# **front が「生きているか」ではなく「自分の app に繋がっているか」を見る。**
	# TCP connect や単なる 200 では足りない — 誤配線した front も listener は
	# 生きていて、相手の app の応答を 200 で返す (#2917 の症状そのもの)。
	# `/api/meta` の `uri` は config.url 由来なので、front ごとに期待する値が違う。
	#
	# network 名は project 名から決まるが、`COMPOSE_PROJECT_NAME` が compose の
	# `name:` を上書きするので**実物から引く**。ハードコードすると、export して
	# いる手元だけ「front が上がらない」と誤診する。
	@echo "verifying each nginx front reaches its own app..."
	@net=$$(docker inspect -f '{{range $$k, $$v := .NetworkSettings.Networks}}{{$$k}}{{end}}' \
		$$(docker compose -f $(QUEUE_BENCH_COMPOSE) ps -q nginx-mkq)); \
	if [ -z "$$net" ]; then echo "could not resolve the bench network" >&2; exit 1; fi; \
	for i in $$(seq 1 30); do \
		bad=""; \
		for h in mk-mkq ts; do \
			got=$$(docker run --rm --network "$$net" curlimages/curl:8.11.1 -sk --max-time 5 \
				-X POST -H 'content-type: application/json' -d '{}' "https://$$h/api/meta" 2>/dev/null \
				| grep -o '"uri":"[^"]*"' | head -1); \
			if [ "$$got" != "\"uri\":\"https://$$h\"" ]; then bad="$$bad $$h(got=$$got)"; fi; \
		done; \
		if [ -z "$$bad" ]; then echo "ready (each front reaches its own app)"; exit 0; \
		fi; \
		sleep 2; \
	done; \
	echo "nginx fronts are not wired to their own apps:$$bad" >&2; \
	echo "hint: nginx resolves its upstream once at startup (#2917)" >&2; exit 1

queue-bench-outbound: ## queue-bench の outbound 計測
	# queue-bench-seed と同じ理由で `--force-recreate` (#1163)。
	docker compose -f $(QUEUE_BENCH_COMPOSE) --profile outbound up --abort-on-container-exit --force-recreate --no-deps driver-outbound

queue-bench-inbound: ## queue-bench の inbound 計測
	# queue-bench-seed と同じ理由で `--force-recreate` (#1163)。
	docker compose -f $(QUEUE_BENCH_COMPOSE) --profile inbound up --abort-on-container-exit --force-recreate --no-deps driver-inbound

queue-bench-report: ## queue-bench のレポートを生成
	# queue-bench-seed と同じ理由で `--force-recreate` (#1163)。
	docker compose -f $(QUEUE_BENCH_COMPOSE) --profile report up --abort-on-container-exit --force-recreate --no-deps report

queue-bench-all: queue-bench-seed queue-bench-outbound queue-bench-inbound queue-bench-report ## queue-bench を一通り実行

queue-bench-down: ## queue-bench スタックを撤去
	# **`--remove-orphans` では profile container は消えない。** あれが消すのは
	# compose ファイルに定義が無い container で、profile 付きは「定義はあるが
	# 有効でない」だけなので対象外 (実測で確認)。profile を明示する必要がある。
	docker compose -f $(QUEUE_BENCH_COMPOSE) \
		--profile bench --profile outbound --profile inbound --profile report \
		down -v --remove-orphans

queue-bench-logs: ## queue-bench のログを表示
	docker compose -f $(QUEUE_BENCH_COMPOSE) logs -f

# Auto-scale comparison bench (#1126 / #1120 tracker).
# 3 scenario (fixed16 / fixed64 / auto) を同一 mkq stack で逐次実行し、
# drain time / Redis client count を比較する。queue-bench との同居・
# 並列実行は想定しない (port は publish していないが volume / network 名は
# 別)。詳細: tests/bench/queue-autoscale/README.md (or docs/queue-bench.md)
AUTOSCALE_BENCH_DIR=tests/bench/queue-autoscale

queue-bench-autoscale-run: ## worker 数 fixed16 / fixed64 / auto を比較実行
	cd $(AUTOSCALE_BENCH_DIR) && ./run.sh

queue-bench-autoscale-down: ## autoscale ベンチのスタックを撤去
	cd $(AUTOSCALE_BENCH_DIR) && docker compose down -v --remove-orphans

queue-bench-autoscale-logs: ## autoscale ベンチのログを表示
	cd $(AUTOSCALE_BENCH_DIR) && docker compose logs -f

# Playwright e2e (#744 Phase 1)
#
# upstream Misskey TS 互換挙動を期待値に書いた spec を mk-go backend に
# 対して走らせ、drop-in 互換 regression を検出する。Phase 1 PR-1 では
# 基盤 + smoke 1 spec のみ。後続 PR で spec 拡充 + CI 統合する。
PLAYWRIGHT_COMPOSE=tests/playwright/compose.yml

##@ e2e: Playwright
playwright-up: ## Playwright スタック (mk-go backend) を起動
	docker compose -f $(PLAYWRIGHT_COMPOSE) up -d --build

# PLAYWRIGHT_ARGS は runner の `playwright` に素通しする追加引数。CI が
# `--shard=i/N` を渡してシャード分割するために使う (#2609)。ローカルで
# 動画が欲しいときは `PLAYWRIGHT_ARGS=--video=on` を渡す。
#
# runner の ENTRYPOINT は `playwright` で CMD が `test`。引数を足すと CMD が
# 丸ごと置き換わるので、`test` を明示してから追加する。
PLAYWRIGHT_ARGS ?=

playwright-test: ## Playwright spec を実行 (mk-go backend、PLAYWRIGHT_ARGS で引数追加)
	# `--build` を付けて runner image を rebuild check させる。package.json
	# 更新時に node_modules が古いままにならないよう、毎回 build context を
	# 確認する (cache hit なら ms 単位で済むので overhead 無視可)。
	docker compose -f $(PLAYWRIGHT_COMPOSE) --profile test run --rm --build playwright-runner test $(PLAYWRIGHT_ARGS)

playwright-down: ## Playwright スタックを撤去
	docker compose -f $(PLAYWRIGHT_COMPOSE) --profile test down -v

playwright-logs: ## Playwright スタックのログを表示
	docker compose -f $(PLAYWRIGHT_COMPOSE) logs -f

# Playwright TS validation (#744 Phase 1)
#
# 同 spec を upstream Misskey TS image (= 真の互換挙動の baseline) に対しても
# 走らせる。両方で pass = drop-in 互換が確認される、片方のみ pass = drift /
# spec 誤りとして調査対象。
PLAYWRIGHT_TS_OVERLAY=tests/playwright/compose.ts.yml

playwright-ts-up: ## Playwright スタック (Misskey TS backend) を起動
	docker compose -f $(PLAYWRIGHT_COMPOSE) -f $(PLAYWRIGHT_TS_OVERLAY) up -d --build

playwright-ts-test: ## Playwright spec を実行 (TS backend、upstream 追従時のみ)
	# `playwright-test` と同じく `--build` で runner image を最新化する。
	#
	# **`specs/upstream` に絞る。** `specs/mkgo` は mk-go 独自機能の spec で、
	# 公式 image に対しては通らない (README の「境界」を参照)。絞らないと
	# TS backend 実行が mkgo 側の spec で落ちる。
	docker compose -f $(PLAYWRIGHT_COMPOSE) -f $(PLAYWRIGHT_TS_OVERLAY) --profile test run --rm --build playwright-runner test specs/upstream $(PLAYWRIGHT_ARGS)

playwright-ts-down: ## Playwright TS スタックを撤去
	docker compose -f $(PLAYWRIGHT_COMPOSE) -f $(PLAYWRIGHT_TS_OVERLAY) --profile test down -v

# Differential e2e diff harness (#2089) ― mk-go と Misskey TS を同一版で
# 並列に立て、同一 endpoint のレスポンスを diff して entitycompat
# golden gate がカバーしない値レベル乖離を検出する。詳細は docs/diff-e2e.md。
# 隔離 stack (own network/volumes)、production UDS には触れない。
DIFF_COMPOSE=tests/diff/compose.yml

##@ e2e: 差分比較ハーネス
diff-up: ## 差分比較ハーネスのスタックを起動
	docker compose -f $(DIFF_COMPOSE) up -d --build

diff-test: ## mk-go ↔ TS の値レベル diff を実行
	docker compose -f $(DIFF_COMPOSE) --profile test run --rm --build diff-runner

diff-down: ## 差分比較ハーネスのスタックを撤去
	docker compose -f $(DIFF_COMPOSE) --profile test down -v

diff-logs: ## 差分比較ハーネスのログを表示
	docker compose -f $(DIFF_COMPOSE) logs -f

# Misskey 本家の backend e2e (test/e2e/**) を mk-go に向けて実行する。
# テスト本体には手を入れず、本家の取得先 (.cache/misskey) の vitest 設定 2 ファイル
# (globalSetup / setupFiles) だけを差し替えている。上流でテストが増えれば
# 自動的にこちらの検証対象も増える。詳細は docs/upstream-backend-e2e.md。
#
# 『通らないことが正しい』テストは tests/upstream-e2e/known-divergences.json に
# 根拠付きで登録し、vitest の expected-failure として扱う。乖離が解消して通る
# ようになったテストは逆に落ちるので、一覧が陳腐化しない。
#
# ポートは本家 .github/misskey/test.yml に合わせてある (54312 / 56312 / 61812)。
UPSTREAM_E2E_COMPOSE=tests/upstream-e2e/compose.yml
UPSTREAM_E2E_CONFIG=tests/upstream-e2e/mkgo.yml
# 本家そのもの (`make upstream-fetch` の取得先) で走らせる (#3378)。mk-go へ向ける
# ための 3 ファイルは tests/upstream-e2e/harness/ に置き、upstream-e2e-test が
# 実行のたびに本家の packages/backend/ へコピーする (編集がすぐ効くように deps
# ではなく test の側で写す)。vitest の設定を本家の外に置いたまま本家のテストを
# 走らせる形は採らない — 設定ファイルの import (`vitest/config`、本家の
# `./vitest.config.js`) は設定ファイルの場所から解決されるので、tests/ には
# node_modules が無く失敗する。symlink も vite が実体のパスへ解決するので同じ。
UPSTREAM_E2E_MISSKEY=$(UPSTREAM_DIR)
UPSTREAM_E2E_HARNESS=tests/upstream-e2e/harness
UPSTREAM_E2E_BACKEND=$(UPSTREAM_E2E_MISSKEY)/packages/backend

##@ 本家 (比較対象)
# 比較対象の本家 Misskey は `.cache/misskey/<版>/` から読む
# (#3378)。版は UPSTREAM_MISSKEY_VERSION の 1 行で、tools とテストは
# internal/upstreamsrc 経由で同じ場所を見る。MK_UPSTREAM_DIR で場所を変えられる。
#
# **`$(shell)` は使わない** (gaterun-check の前提、REVISION_LDFLAGS の注記を参照)。
# 版は `$(file <...)` で読む。ファイルを読むだけで、`make -pn` に副作用は無い。
UPSTREAM_MISSKEY_VERSION := $(file <UPSTREAM_MISSKEY_VERSION)
UPSTREAM_DIR ?= $(if $(MK_UPSTREAM_DIR),$(MK_UPSTREAM_DIR),.cache/misskey/$(UPSTREAM_MISSKEY_VERSION))
# 版ごとの worktree の元になる bare repository。2 回目以降の取得と、追従作業で
# 旧版と新版を並べるときに速い (設計 D2 / Q3)。
UPSTREAM_MIRROR ?= .cache/misskey/mirror.git
UPSTREAM_REMOTE ?= https://github.com/misskey-dev/misskey.git

# 取得済みなら版を確かめて何もしない。**版が違う worktree は上書きしない** —
# 手で直した跡があるかもしれないので、消してから取り直すよう案内して落ちる。
# `worktree prune` は、worktree を `rm -rf` だけで消したときに mirror 側に残る
# 登録を掃除する (残っていると `worktree add` が already registered で落ちる)。
upstream-fetch: ## UPSTREAM_MISSKEY_VERSION の本家を .cache/misskey/<版> へ取得
	@set -e; v="$(UPSTREAM_MISSKEY_VERSION)"; d="$(UPSTREAM_DIR)"; \
	if [ -z "$$v" ]; then echo "UPSTREAM_MISSKEY_VERSION が読めない" >&2; exit 1; fi; \
	if [ -e "$$d/.git" ]; then \
		got=$$(git -C "$$d" describe --tags --exact-match 2>/dev/null || true); \
		if [ "$$got" = "$$v" ]; then echo "upstream $$v: $$d (取得済み)"; exit 0; fi; \
		echo "$$d は $$v ではない ($${got:-tag 無し})。git -C $(UPSTREAM_MIRROR) worktree remove --force $$d で消してから取り直す" >&2; exit 1; \
	fi; \
	if [ -e "$$d" ]; then echo "$$d が git の worktree ではない。消してから取り直す" >&2; exit 1; fi; \
	if [ ! -d "$(UPSTREAM_MIRROR)" ]; then \
		git clone --bare --no-tags "$(UPSTREAM_REMOTE)" "$(UPSTREAM_MIRROR)"; \
	fi; \
	git -C "$(UPSTREAM_MIRROR)" fetch --no-tags origin "refs/tags/$$v:refs/tags/$$v"; \
	git -C "$(UPSTREAM_MIRROR)" worktree prune; \
	case "$$d" in /*) p="$$d" ;; *) p="$(CURDIR)/$$d" ;; esac; \
	git -C "$(UPSTREAM_MIRROR)" worktree add --detach "$$p" "refs/tags/$$v"; \
	echo "upstream $$v: $$d"

# golden が本家の版に追いついているか。本家から作り直して差分が無いことと、
# 本家を読むテストが skip されずに通ることを見る。本家を取得する
# apicompat.yml が回す (本家を読まない `make gates` には入れない)。
upstream-check: ## golden と本家を読むテストが UPSTREAM_MISSKEY_VERSION の本家と一致するか検査
	$(MAKE) shapecheck-gen
	git diff --exit-code -- internal/entitycompat/testdata
	MK_UPSTREAM_REQUIRE=1 go test ./internal/misc/achievement/... -run 'TestTypes_MatchUpstream' -count=1 -v

# 本家の新しい版 (TO=<版>) の差分のうち、frontend/ が取り込むパスだけを 3-way で当てる
# (設計 D4、#3379)。手順の全体は docs/upstream-catch-up.md。
#
#  - mirror (upstream-fetch と共有) に今の版と TO の tag を取ってから、本体の
#    refs/upstream/ へ取り込む。--3way は当てる前の blob を手元で探すため
#  - 変更されたパスを D1 の区分で分け、どれにも当たらないパス (本家が直下に新しく
#    足したものなど) があれば何も当てずに止める
#  - 衝突はファイル単位で衝突マーカーとして残る。pnpm-lock.yaml は当てず、
#    `make upstream-sync-lock` で作り直す
#  - DRY=1 なら分類だけを表示する (DRY に 1 / true / yes 以外を入れても当てる)
upstream-sync: ## 本家の新しい版 (TO=<版>) の frontend 側の差分を frontend/ へ当てる
	@set -e; to="$(TO)"; from="$(UPSTREAM_MISSKEY_VERSION)"; \
	if [ -z "$$to" ]; then echo "TO=<本家の版> を指定する (例: make upstream-sync TO=2026.11.0)" >&2; exit 1; fi; \
	if [ -z "$$from" ]; then echo "UPSTREAM_MISSKEY_VERSION が読めない" >&2; exit 1; fi; \
	if [ ! -d "$(UPSTREAM_MIRROR)" ]; then git clone --bare --no-tags "$(UPSTREAM_REMOTE)" "$(UPSTREAM_MIRROR)"; fi; \
	git -C "$(UPSTREAM_MIRROR)" fetch --no-tags origin "refs/tags/$$from:refs/tags/$$from" "refs/tags/$$to:refs/tags/$$to"; \
	GOWORK=off go run ./tools/upstreamsync -from "$$from" -to "$$to" -source "$(UPSTREAM_MIRROR)" $(if $(filter 1 true yes,$(DRY)),-dry-run)

# frontend/pnpm-lock.yaml を、**直前の lock を基点に** package.json から作り直す (D4)。
# 本家の lock は使わない (backend を外した lock は本家より約 4800 行少なく、本家の lock の
# 差分は毎回衝突する)。--lockfile-only なので node_modules と frontend/built は触らない。
# 作り直した lock の差分を目で見て、package.json で変わった依存以外が動いていないことを
# 確かめる (--lockfile-only は lock に無い依存と範囲が変わった依存をその時点の最新に解決する)。
#
# **store は container の中に置く** (`pnpm_config_store_dir`)。既定のままだと mount の根
# (frontend/) に root 所有の frontend/.pnpm-store ができ、Dockerfile.bundled の
# `COPY frontend/` にも入る。pnpm 11 は `npm_config_*` を読まないので `pnpm_config_*` で
# 渡す (実測)。**コメントは recipe の中に置かない** — 行継続の途中に挟むとそこで shell が
# 分かれ、後ろの docker run から変数が見えなくなる。
upstream-sync-lock: ## frontend/pnpm-lock.yaml を package.json から作り直す (upstream-sync の後)
	@node_ver=$$(tr -d '[:space:]' < frontend/.node-version); \
	pnpm_ver=$$(sed -n 's/.*"packageManager"[[:space:]]*:[[:space:]]*"pnpm@\([^"+]*\).*/\1/p' \
		frontend/package.json | head -1); \
	if [ -z "$$node_ver" ] || [ -z "$$pnpm_ver" ]; then echo "frontend/.node-version か packageManager を読めない" >&2; exit 1; fi; \
	docker run --rm -e CI=true -e pnpm_config_store_dir=/tmp/pnpm-store \
		-v "$(CURDIR)/frontend:/work/frontend" -w /work/frontend \
		"node:$$node_ver-$(FRONTEND_NODE_DISTRO)" \
		bash -lc "npm i -g pnpm@$$pnpm_ver && pnpm install --lockfile-only"

##@ e2e: 本家 backend e2e
# 本家の取得先の依存を用意する。初回と UPSTREAM_MISSKEY_VERSION を上げた後にだけ必要。
#
#  - misskey-js: exports が built/ を指すのでビルドしないと test/e2e が import できない。
#    frontend まで含む `pnpm build` (5-10 分) は e2e には不要なので呼ばない。
#  - .config/test.yml: 本家の utils.ts / setup が loadConfig() 経由で読む (port 等)。
#  - compile-config: loadConfig() は YAML ではなく built/.config.json を読むので、
#    NODE_ENV=test で .config/test.yml から生成しておく必要がある。
#  - build-pre: loadConfig() は built/meta.json も readFileSync する (無いと ENOENT)。
#    frontend の manifest は existsSync 判定なので無くてよい。
upstream-e2e-deps: upstream-fetch ## 本家 backend e2e に必要な本家側の依存を用意 (初回のみ)
	cd $(UPSTREAM_E2E_MISSKEY) && \
		pnpm install --frozen-lockfile && \
		pnpm build-pre && \
		pnpm --filter misskey-js build && \
		cp .github/misskey/test.yml .config/ && \
		NODE_ENV=test pnpm --filter backend compile-config

upstream-e2e-up: ## 本家 backend e2e 用の PostgreSQL / Redis を起動
	docker compose -f $(UPSTREAM_E2E_COMPOSE) up -d --wait

upstream-e2e-migrate: ## e2e 用 DB にマイグレーションを適用
	go run ./cmd/elythia migrate -config $(UPSTREAM_E2E_CONFIG) -direction up

# FILE で 1 ファイルだけ流せる: make upstream-e2e-test FILE=test/e2e/note.ts
# VITEST_ARGS は vitest に素通しする追加引数。CI が `--shard=i/N` を渡して
# シャード分割するために使う (#2609)。
#
# **プロセス内で並列にはできない。** upstream の vitest 設定が `maxWorkers: 1`
# で、かつ setupFiles がファイルごとに mk-go の `/api/reset-db` (全テーブル
# truncate) を叩く。同じ DB に 2 ファイルを並行させると片方が相手のフィクスチャ
# を実行中に消す。シャードごとに DB を分けるのが前提。
VITEST_ARGS ?=

# **mk-go が配る静的なファイルも本家の取得先から取る** (#3378)。favicon や絵文字の
# 画像 (`test/e2e/fetch-resource.ts` が見る) は、既定では frontend/assets と、
# frontend/ へ pnpm install した emoji-assets から配る。この e2e は frontend/ に
# pnpm install しない (CI) ので、本家の取得先 (upstream-e2e-deps が
# pnpm install 済み) を環境変数で指す。値は mk-go の cwd (MKGO_CWD = リポジトリ
# 直下) から解決される。entry.ts は環境変数をそのまま mk-go へ渡す。
UPSTREAM_E2E_ASSETS_ENV = \
	MISSKEY_STATIC_DIR=$(UPSTREAM_E2E_BACKEND)/assets \
	MISSKEY_REPO_ASSETS_DIR=$(UPSTREAM_E2E_MISSKEY)/assets \
	MISSKEY_TWEMOJI_DIR=$(UPSTREAM_E2E_BACKEND)/node_modules/@misskey-dev/emoji-assets/built/twemoji \
	MISSKEY_FLUENT_EMOJI_DIR=$(UPSTREAM_E2E_BACKEND)/node_modules/@misskey-dev/emoji-assets/built/fluent-emoji

upstream-e2e-test: build ## 本家 backend e2e を mk-go に対して実行 (VITEST_ARGS で引数追加)
	cp -R $(UPSTREAM_E2E_HARNESS)/. $(UPSTREAM_E2E_BACKEND)/
	cd $(UPSTREAM_E2E_BACKEND) && \
		$(UPSTREAM_E2E_ASSETS_ENV) \
		MKGO_BIN=$(CURDIR)/built/$(BINARY) \
		MKGO_CONFIG=$(CURDIR)/$(UPSTREAM_E2E_CONFIG) \
		MKGO_CWD=$(CURDIR) \
		npx --no vitest run --config vitest.config.e2e.mkgo.ts $(VITEST_ARGS) $(FILE)

upstream-e2e: upstream-e2e-deps upstream-e2e-up upstream-e2e-migrate upstream-e2e-test ## 依存の用意からテストまで一括で実行

upstream-e2e-down: ## 本家 backend e2e 用のスタックを撤去 (volume ごと)
	docker compose -f $(UPSTREAM_E2E_COMPOSE) down -v

# API compatibility matrix ― mk-go と Misskey TS の API endpoint 実装状況を
# 突き合わせて docs/api-compat.md を生成する。
#
# - APICOMPAT_TS_DIR: TS endpoints ディレクトリ。本家の取得先 (`make upstream-fetch`) に依存。
# - APICOMPAT_CONFIG: dump-routes 時に読み込む mk-go config。DB/Redis 接続
#   は必須なので、docker compose up された stack を持っていることが前提。
# - APICOMPAT_ROUTES: dump-routes が書き出す中間ファイルの path。
#   `$(BUILD_DIR)` 配下にして hermetic に保つ ( /tmp 共有事故を避ける)。
APICOMPAT_TS_DIR    ?= $(UPSTREAM_DIR)/packages/backend/src/server/api/endpoints
# fastify 直登録 endpoint (signup / signin-flow / miauth check / instance peers)
# の抽出元。endpoints/ の file-walk では拾えないので source から直接読む。
APICOMPAT_TS_DIRECT ?= $(UPSTREAM_DIR)/packages/backend/src/server/api/ApiServerService.ts
APICOMPAT_CONFIG    ?= .config/default.yml
APICOMPAT_ROUTES    ?= $(BUILD_DIR)/apicompat-routes.json
APICOMPAT_OUT       ?= docs/api-compat.md

# mk-go binary を build → `elythia dump-routes` で route 一覧を JSON dump。
# DB / Redis 接続を必要とするので make docker-up 等で stack を立てた状態で
# 実行すること。
apicompat-routes: build ## route 一覧を JSON dump (stack 起動が必要)
	mkdir -p $(dir $(APICOMPAT_ROUTES))
	$(BUILD_DIR)/$(BINARY) dump-routes -config $(APICOMPAT_CONFIG) -dump-routes-out $(APICOMPAT_ROUTES)

# 既存 APICOMPAT_ROUTES JSON だけ comparator にかけて matrix を再生成する
# (DB / Redis 接続不要)。matrix の format / category 表示を iterate する時に
# 毎回 build + dump し直さなくて済むよう用意した escape hatch。前提として
# `apicompat-routes` を最低一度走らせていること。
apicompat-render: ## route dump から互換性マトリクスを生成
	go run ./tools/apicompat \
		-ts-endpoints-dir $(APICOMPAT_TS_DIR) \
		-ts-api-server-service $(APICOMPAT_TS_DIRECT) \
		-mk-routes $(APICOMPAT_ROUTES) \
		-out $(APICOMPAT_OUT)
	@echo "wrote $(APICOMPAT_OUT)"

# routes JSON + TS endpoints ディレクトリを comparator で突き合わせて matrix
# を生成する。`apicompat-routes` を都度先に走らせて、stale な中間 JSON で
# matrix を作らないようにする (`.PHONY` 効果で常に build + dump し直す)。
# 反復 iterate で DB 接続を毎回避けたい場合は `apicompat-render` を使う。
apicompat: apicompat-routes apicompat-render ## API 互換性マトリクス docs/api-compat.md を生成

# --- entity shape drift (Layer 0 static API compatibility) ------------------
# mk-go の entity DTO struct を Misskey contract (misskey-js types.ts) と
# フィールド単位で突き合わせ、shape drift (欠落 / null性 / optional性) を検出する。
# サーバー / ブラウザ / Docker 不要で、CI では `go test ./...` 内の
# TestEntityShapeDrift gate として自動実行される。詳細は docs/shape-drift.md。

# golden snapshot (testdata/golden_schemas.json + golden_error_ids.json) を
# 本家 (`make upstream-fetch` の取得先) から再生成する。UPSTREAM_MISSKEY_VERSION
# を上げたら必ず実行し、生成された snapshot を commit すること。
##@ 静的 parity ゲート (サーバー・Docker 不要)
shapecheck-gen: ## shape drift の golden snapshot を再生成
	go run ./tools/shapediff
	go run ./tools/erroriddiff
	go run ./tools/limitspec
	go run ./tools/permspec
	go run ./tools/securespec
	go run ./tools/schemadrift

# 全 family の drift を severity 付きで一覧表示する (gate にかける前の調査用)。
shapecheck-report: ## shape drift のレポートを出力
	go run ./tools/shapediff -report

# drift gate (L0 静的 + L2 実行時) をローカルで実行する (CI と同じ判定)。
# L0: TestEntityShapeDrift / L2: Test*ShapeL2 (Notification / Announcement / ...)。
shapecheck: ## レスポンス形状の drift を検査
	go test ./internal/entitycompat/... -run 'TestEntityShapeDrift|ShapeL2' -count=1 -v

# error-id / error-HTTP-status / error-kind drift gate をローカルで実行する。
# handler が emit する error id (inline / UUID 定数 / apierr helper / echo
# wrapper) と、Misskey が明示する HTTP status / kind discriminator を router
# 経由で endpoint に解決して突合する静的 gate。詳細は docs/shape-drift.md。
errorid-check: ## error id / HTTP status / kind の drift を検査
	go test ./internal/entitycompat/... -run 'TestErrorIDDrift|TestErrorHTTPStatusDrift|TestErrorKindDrift' -count=1 -v

# pagination limit-spec drift gate をローカルで実行する。handler が
# pagination.ClampLimit(limit, def, max) で渡す default/max literal を router
# 経由で endpoint に解決し、Misskey paramDef の default/maximum と突合する。
limitspec-check: ## ページネーションの default / max の drift を検査
	go test ./internal/entitycompat/... -run TestLimitSpecDrift -count=1 -v

# permission drift gate をローカルで実行する。mk-go の router middleware が
# Misskey の requireAdmin/requireModerator/requireCredential より緩くないか検証。
# TestOAuthKindDrift は別軸で、meta.kind (OAuth scope) の一致を見る。
# **アクセス階層の gate では scope の取り違えを検出できない** (#2877)。
perm-check: ## router middleware の権限が upstream より緩くないか検査
	go test ./internal/entitycompat/... -run 'TestPermissionDrift|TestSecureDrift|TestOAuthKindDrift' -count=1 -v

.PHONY: wiring-check
wiring-check: ## router で配線が必要なものが外れていないか検査
	go test ./internal/entitycompat/... -run 'TestTimelineTogglesAreWired|TestSecurityHeadersAreWired|TestCriticalWiringCountMatchesTable|TestInviteModeratorCheckerIsWired|TestPluginPeerBodyLimitIsWired|TestPluginPeerRateLimiterIsWired|TestAPICatchallIsWired|TestPluginJobQueuesAreWired|TestPluginPeerEnqueuerIsWired|TestReadAllNotificationsPusherIsWired|TestWebPushProducersAreWired|TestChatPusherIsWired|TestChartManagementLoggerIsResolvedAtWiring|TestNotificationPolicyResolverIsWired|TestAbuseReportNotifierIsWired|TestNotificationModeratorCheckerIsWired|TestAbuseReportLookupIsWired|TestNormalizeWiringKeepsStringLiteralSpacing|TestEmojiDecorationCacheIsWired|TestEmojiMutationsDropDecorationCache|TestApplicationReceivedNotificationIsWired|TestMediaProxyConcurrencyIsWired|TestCaptchaReloadIsWired|TestRoleInvalidationIsWired|TestCredentialRoutesWithoutScopeRejectAppTokens|TestAppTokenGateExemptHasNoDeadEntries|TestOutboundConstructorsReceiveSharedOptions|TestPeerJobEnvelopeTagIsStable|TestPrivilegedPolicyKeysMatchAdminRoutes|TestStripGoComments|TestCleanProcessorReceivesThePendingPruner|TestDriveUsageProviderIsWired|TestDriveUsageRouteIsRegistered|TestIPLogServiceIsWired|TestClientIPMiddlewareIsWired|TestSigninIPRecorderIsWired|TestIPAccountSearchRepoIsWired|TestIPAccountSearchRouteIsRegistered|TestIPRelatedAccountsRouteIsRegistered|TestIPLookupAuditIsWired|TestIPLookupLogRetentionIsWired|TestIPLookupLogRouteIsRegistered|TestIPLookupRoutesHaveRateLimits|TestPasswordChecksAreFailureLimited|TestPasswordFailureGuardIsWired|TestRemoteStatsGateUsesFailClosedPredicate|TestStreamRevokeIsWired|TestAvatarDecorationRoleSetsAreWired|TestPushSubscriptionCacheIsShared|TestInstanceStatsGateIsWired' -count=1 -v

.PHONY: notiftype-check
notiftype-check: ## 通知タイプの一覧が 1 箇所から導出されているか検査
	go test ./internal/core/notification/ -run 'TestRegistryCoversEveryTypeConstant|TestRegistryKindsAreConsistent|TestMkGoTypesAreNotInUpstreamCoverage' -count=1 -v
	go test ./internal/api/notifications/ -run 'TestTypeListsAreDerivedFromRegistry|TestExcludeAllUpstreamTypesCoversEverything' -count=1 -v

.PHONY: migrationdoc-check
migrationdoc-check: ## migration の本数を述べた doc が実態と合っているか検査
	go test ./internal/entitycompat/... -run 'TestMigrationCountsInDocsMatchReality|TestMigrationCountClaimsDoNotPointIntoHistory|TestClaimPointsIntoHistory|TestNoopDownMigrationListMatchesReality|TestDestructiveMigrationTableRowsAreUnique' -count=1 -v

.PHONY: mdtable-check
mdtable-check: ## md の表の各行がヘッダと同じ列数か検査 (溢れたセルは描画時に捨てられる)
	# GFM は溢れたセルを黙って捨てるので、ソースに書いた内容が GitHub 上で
	# 読めなくなる。原因はほぼセル区切りとして働くパイプで、**コードスパンの
	# 中でも働く** (`\|` へエスケープする)。#2930 で実際に踏んだ。
	# **見るのは列数だけ。** 取りこぼす形はテストの doc コメントに明記してある。
	go test ./internal/entitycompat/... -run 'TestMarkdownTablesDoNotDropContent' -count=1 -v

.PHONY: secretfield-check
secretfield-check: ## モデルの秘密フィールドが json:"-" を保っているか検査
	# モデルをそのまま JSON 化する経路があるので、`json:"-"` が唯一の防波堤に
	# なっているフィールドがある。実測で model.User.Token のタグを外しても
	# make gates も全テストも緑のままだった (native token が取れると、その
	# ユーザーとして API を叩けるので権限ゲートを全て迂回できる)。
	# 出してよいものは serializableSecretLike に理由付きで登録する。
	go test ./internal/entitycompat/... -run 'TestScanSecretLikeFields|TestModelSecretFieldsAreNotSerialized|TestSerializableSecretLikeHasNoDeadEntries|TestModelJSONDoesNotContainSecrets|TestModelJSONKeepsAuditedFields' -count=1 -v

.PHONY: ipshape-check
ipshape-check: ## レスポンス / 連合の shape に IP が出ていないか検査
	# #3066 の「IP 情報が一般ユーザー向け API や連合へ露出しない」を直接見る。
	# 担保が shapecheck の golden 照合しか無く、additive な追加は素通りしていた。
	# **AST で全 struct のタグを読む** — reflect で型を並べる形は `MeDetailed`
	# (= /api/i) を落とし、入れ子や map の値型も辿れていなかった (実測)。
	# 走査は entity / activitypub だけでなく api / server / stream も見る
	# (handler が自分で宣言する response struct もそのまま wire の形になる)。
	# 判定は語で見る。**切り方を片側に寄せると必ず穴が開く** — 大文字のたびに
	# 割ると `lastIPs` が、割らないと `IPAddr` が素通りする (両方とも実測)。
	go test ./internal/entitycompat/... -run 'TestResponseAndFederationShapesHaveNoIPField|TestIPShapeAllowlistMatchesExpected|TestPublicShapesDoNotReferenceIPBearingTypes|TestIPRefAllowlistMatchesExpected|TestIPBearingTypesPinsEveryBranch|TestTypeRefsResolvesNamedTypes|TestAllJSONKeysWalksNestedObjects|TestPublicShapesMarshalWithoutIP|TestScanJSONTagsCollectsWhatEncodingJSONEmits|TestCustomJSONMarshalersAreKnown|TestLooksLikeIPKey' -count=1 -v
.PHONY: iprecord-check
iprecord-check: ## 利用者の IP を記録する call site が allowlist の外に増えていないか検査
	# #3105 の関連アカウント検索は `user_ip` の観測だけを見るので、**失敗した
	# サインインの IP がそこに入ると第三者が他人の関連候補を作れる**。
	# 「どこからも呼ばれていない」は構造的な性質で、endpoint ごとの振る舞い
	# テストは叩いた経路しか見ない (実測で `SigninFlow` と signin-with-passkey に
	# 記録を足す変異が素通りした)。
	go test ./internal/entitycompat/... -run 'TestIPRecordCallSitesAreAllowlisted|TestRecordSuccessfulSigninCallSitesAreAllowlisted|TestPasskeyIPRecordComesAfterFailures' -count=1 -v

.PHONY: sqlbind-check
sqlbind-check: ## 値をクォート内へ差し込まずバインドしているか検査
	# 列名やテーブル名の解決で fmt.Sprintf は要るので、書式を組むこと自体は残る。
	# **書式動詞がクォートで開いたリテラルの内側に在る**形だけを禁じる。
	# **「これは SQL か」は判定しない** — キーワードで判定すると両方向に壊れる。
	# 普通の英文が部分一致で SQL 扱いされて事実と逆の診断が出る一方、
	# `'%s'::varchar[]` のような断片はキーワードに当たらず収集すらされない (実測)。
	# 書式は定数連結を畳んでから見る (折り返すと丸ごと検査対象から消えるため)。
	# chart の unique 配列は別に名指しで見る。**ApplyDeltas が組む書式集合を
	# そのまま pin する** — 「プレースホルダが在るか」はダミーの ? 1 つで満たせる。
	go test ./internal/entitycompat/... -run 'TestSQLBindVerbDetectionShapes|TestSQLBindFoldsConcatenatedFormats|TestSQLBindNoVerbInsideQuotedLiteral|TestSQLBindChartUniqueArrayIsParameterised' -count=1 -v

.PHONY: dockerignore-check
dockerignore-check: ## .dockerignore がシークレットと利用者データを除外しているか検査
	# .dockerignore は全 build context 共通なので、1 行落ちると全経路に同時に効く。
	# #2942 で drive-files (既定の drive の置き場所) と operator-local な設定
	# (.config/*.yml 等) が抜けていた。**配る image には入らない** (最終 stage が
	# 明示パスの COPY しか持たないため) が、build context と builder stage の
	# layer には入り、cache-to を設定していればキャッシュ経由で読める。
	go test ./internal/entitycompat/... -run 'TestDockerignore' -count=1 -v

.PHONY: pluginembed-check
pluginembed-check: ## mk-go をビルドする Dockerfile が pluginbuild を go build より前に実行するか検査
	# 組み込みを忘れた image は **エラーにならない** — plugins/ に置いたのに
	# 入っていない mk-go が黙って出来る。#2940 で Dockerfile.bundled が実際に
	# そうなっていた。生成が go build の後でも同じ結果になるので順序も見る。
	# 検出は動詞 (go build / go install) と対象 (cmd/elythia / cmd/...) の共起で
	# 行い、行継続は畳んでから判定する。組み込まない Dockerfile は理由付きで
	# allowlist に登録する。
	go test ./internal/entitycompat/... -run 'TestDockerfilesEmbedPlugins' -count=1 -v

.PHONY: gaterun-check
gaterun-check: ## gates の -run が名指しするテストが実在するか検査
	go test ./internal/entitycompat/... -run 'TestGateRunPatternsResolve' -count=1 -v

.PHONY: testflags-check
testflags-check: ## make test が CI と同じテスト条件で走るか検査
	go test ./internal/entitycompat/... -run 'TestMakeTestMatchesCIConditions|TestDocsQuoteTheCIShuffleSeed' -count=1 -v

.PHONY: compose-check
compose-check: ## 配布する compose のログの上限と、検証用 compose の置き場所・相対パス・name: を検査
	go test ./internal/entitycompat/... -run 'TestComposeServicesHaveLogLimits|TestRootComposeFilesAreOperatorOnly|TestTestComposeFilesAreSelfContained|TestTestComposeUntrackedSourcesAreUsed' -count=1 -v

.PHONY: catalog-check
catalog-check: ## システムカタログのクエリが schema で絞られているか検査
	go test ./internal/entitycompat/... -run 'TestCatalogQueriesAreSchemaScoped|TestCatalogQueryGate_Predicates' -count=1 -v

.PHONY: notfound-check
notfound-check: ## repository の lookup error を種別を見ずに 4xx にしていないか検査
	go test ./internal/entitycompat/... -run 'TestRepoErrorsAreNotCollapsed|TestScanCollapsedLookups|TestBodyReturnsNotFoundSentinel|TestNotFoundGateWalks' -count=1 -v

.PHONY: nulparam-check
nulparam-check: ## 列に入らない値 (NUL) が SQL の bind parameter に載らないか検査
	go test ./internal/entitycompat/... -run 'TestCursorGuardsAreChecked|TestScanCursorGuards|TestCursorParamsAreNormalized|TestScanCursorParamBinders|TestRepositoryLookupsRejectUnstorableValues|TestScanRepoLookupGuards|TestLikePatternsRejectUnmatchableInput|TestScanLikePatternGuards' -count=1 -v
