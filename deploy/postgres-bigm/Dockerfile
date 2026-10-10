# pg_bigm 入り PostgreSQL 18 (#2514)。
#
# pg_bigm は 2-gram の GIN index (`gin_bigm_ops`) で LIKE 部分一致を加速する
# 拡張。日本語のように単語境界の無い言語の部分一致検索に効く。mk-go の
# sqlLike 検索は upstream と同じ `lower(text) LIKE ...` 形なので、
# 拡張とインデックスを作るだけで provider の変更なしに index が効く
# (手順は docs/deployment.md)。
#
# 公式 image に pg_bigm は無いのでソースからビルドする。alpine の postgres
# image は pgxs (拡張ビルド基盤) を同梱しており、build deps を一時導入して
# make するだけでよい (icu-dev は pg_locale.h が要求。--virtual で削除され
# runtime には残らない)。
#
# **with_llvm=no で JIT 用 bitcode の emit を止める** (#2523)。bitcode には
# clang/llvm が要るが、alpine の clangNN パッケージ名は base image の更新で
# 変わり、実際に clang19 が消えて新規環境のビルドが落ちた。bitcode は JIT が
# 拡張関数をインライン展開するための任意成果物で、pg_bigm の GIN 検索の
# 実性能には影響しない。依存を落として base 更新に頑健な方を取る。
#
# **release tag ではなく commit SHA に固定する。** tag は force-push で
# 差し替え可能だが、commit archive は immutable。SHA は v1.2-20250903
# (PostgreSQL 18 を公式サポートした release) を指す。
FROM postgres:18-alpine

# 上げるときは、ルートの Dockerfile の backup stage (バックアップを戻す側) も同じ値にする。
ARG PG_BIGM_COMMIT=735dceba0ecdd8ac1aaaaa207226a7102b6bbd71

RUN apk add --no-cache --virtual .build-deps build-base curl icu-dev \
 && curl -fsSL "https://github.com/pgbigm/pg_bigm/archive/${PG_BIGM_COMMIT}.tar.gz" -o /tmp/pg_bigm.tar.gz \
 && tar xzf /tmp/pg_bigm.tar.gz -C /tmp \
 && make -C "/tmp/pg_bigm-${PG_BIGM_COMMIT}" USE_PGXS=1 with_llvm=no install \
 && rm -rf /tmp/pg_bigm* \
 && apk del .build-deps
