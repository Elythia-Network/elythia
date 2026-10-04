#!/bin/bash
#
# 配信中の frontend アセットが実在するかを検証する。
#
# mk-go は DetectClientEntry で entry point を起動時に 1 回だけ解決して
# キャッシュする (internal/server/frontend.go)。frontend を再ビルドしても
# mk-go を再起動しないと、消えた古いハッシュを指したままになりフロントエンドが
# 起動しない。2026-09-07 に本番で 10 分近くこれを踏んでいる (#2885)。
#
# **CSS も見る。** vite のファイル名は内容ハッシュなので、内容が変わらない
# ファイルは再ビルドしても同じ名前で作り直される。逆に CSS だけ内容が変われば
# CSS の名前だけが変わるので、entry の JS だけ見ると取り逃がす。
#
# **ブラウザが読むのは scripts/ ではない。** index の loader は
# `CLIENT_ENTRY.replace('scripts', lang)` で言語ごとのパスへ振り替えるので、
# LANGS の各言語について実在を確かめる。
#
# **キャッシュを避ける。** 公開 URL は CDN の裏にいることがあり、直前まで
# 配信していた古いアセットはエッジに残っている。素で叩くと、まさにこの
# スクリプトが検出したい状況で 200 (キャッシュヒット) が返る。
#
# usage: check-frontend-entry.sh <config.yml> [timeout-seconds]
set -u

config=${1:-}
timeout=${2:-120}

if [ -z "$config" ]; then
	printf 'usage: %s <config.yml> [timeout-seconds]\n' "$0" >&2
	exit 2
fi
if [ ! -f "$config" ]; then
	printf '\033[31m==> 設定ファイルが無い: %s\033[0m\n' "$config" >&2
	exit 2
fi
case $timeout in
	*[!0-9]* | '') printf '\033[31m==> timeout は秒数で指定する: %s\033[0m\n' "$timeout" >&2; exit 2 ;;
esac

# `url: https://example.com/  # 本番` のような行末コメントと余白を落とす。
# MK_URL は yml の url を上書きする (CLAUDE.md Section 9)。export されて
# いればそちらを見ないと、別の URL を検査して偽の緑 / 赤を出す。
url=${MK_URL:-$(sed -n 's/^url:[[:space:]]*//p' "$config" | head -1 | tr -d '\r')}
url=${url%%#*}
url=$(printf '%s' "$url" | sed 's/[[:space:]]*$//')
url=${url%\"}; url=${url#\"}
url=${url%\'}; url=${url#\'}
url=${url%/}

if [ -z "$url" ]; then
	printf '\033[31m==> url を決められない (%s に url: が無く MK_URL も未設定)\033[0m\n' "$config" >&2
	exit 1
fi

# キャッシュを外すための使い捨てクエリ。
cb="mkcheck=$(date +%s)-$$"

fetch_code() { # <path>
	local sep='?'
	case $1 in *\?*) sep='&' ;; esac
	curl -sL -o /dev/null -w '%{http_code}' --max-time 15 "$url$1$sep$cb"
}

# 再起動直後は nginx が 502 を返すので、index が取れるまでは待つ。
# アセットが 404 なのは manifest が古いということなので、そちらは待たない。
index=''
deadline=$(( $(date +%s) + timeout ))
while :; do
	if index=$(curl -fsSL --max-time 10 "$url/?$cb" 2>/dev/null) && [ -n "$index" ]; then
		break
	fi
	if [ "$(date +%s)" -ge "$deadline" ]; then
		printf '\033[31m==> index を取得できない (%s 秒待った): %s/\033[0m\n' "$timeout" "$url" >&2
		exit 1
	fi
	sleep 2
done

entry=$(printf '%s' "$index" | sed -n "s/.*CLIENT_ENTRY = '\([^']*\)'.*/\1/p" | head -1)
if [ -z "$entry" ]; then
	if printf '%s' "$index" | grep -q 'CLIENT_ENTRY = null'; then
		printf '\033[31m==> CLIENT_ENTRY が null: ビルド済みアセットが見えていない\033[0m\n' >&2
		printf '    frontend/built を作ってから (make uds-frontend-build) mk-go を再起動すること。\n' >&2
		exit 1
	fi
	printf '\033[31m==> index から CLIENT_ENTRY を取り出せない (書式が変わった?): %s/\033[0m\n' "$url" >&2
	exit 1
fi

# index の loader が実際に組み立てる URL を作る。**読めなかったら落とす** —
# 素の scripts/<hash>.js はブラウザが一度も要求しないパスなので、そこだけ見て
# 緑を返すのは「検査していないのに緑」と同じ。
langs=$(printf '%s' "$index" | sed -n 's/.*LANGS = \[\([^]]*\)\].*/\1/p' | head -1 | tr -d '"'"'"' ' | tr ',' ' ')
if [ -z "$langs" ]; then
	printf '\033[31m==> index から LANGS を取り出せない (書式が変わった?): %s/\033[0m\n' "$url" >&2
	exit 1
fi

paths=("/vite/$entry")
for lang in $langs; do
	paths+=("/vite/${entry/#scripts\//$lang/}")
done

# index が読み込む stylesheet も同じ manifest 由来なので一緒に見る。
css_count=0
while IFS= read -r css; do
	[ -n "$css" ] || continue
	paths+=("$css")
	css_count=$((css_count + 1))
done < <(printf '%s' "$index" | grep -oE 'href="/vite/[A-Za-z0-9._/-]+\.css"' | sed 's/^href="//; s/"$//' | sort -u)

# stylesheet があるはずなのに 1 本も拾えないのは、書式が変わって正規表現が
# 空振りしたということ。黙って対象を減らさない。
if [ "$css_count" -eq 0 ] && printf '%s' "$index" | grep -q 'rel="stylesheet"'; then
	printf '\033[31m==> stylesheet の href を取り出せない (書式が変わった?): %s/\033[0m\n' "$url" >&2
	exit 1
fi

failed=0
checked=0
for p in "${paths[@]}"; do
	code=$(fetch_code "$p")
	checked=$((checked + 1))
	if [ "$code" != 200 ]; then
		printf '\033[31m==> %s: %s%s\033[0m\n' "$code" "$url" "$p" >&2
		failed=$((failed + 1))
	fi
done

if [ "$failed" -ne 0 ]; then
	printf '\033[31m==> %d/%d のアセットが配信されていない\033[0m\n' "$failed" "$checked" >&2
	printf '    frontend を再ビルドしたあと mk-go を再起動していない可能性が高い。\n' >&2
	exit 1
fi

printf '\033[32m==> frontend アセット OK: %s ほか %d 件\033[0m\n' "$entry" "$((checked - 1))"
