#!/bin/sh
#
# 拡張を作ってから golang-migrate を実行する。
#
# **サブコマンドを引数で受ける。**
# `up` を焼き込んでしまうと、**失敗して dirty になったときに
# force が打てない** —— コンテナに入る手段も無いので、
# イメージを作り直すまで復旧できなくなる (実際に踏んだ)。
#
#   既定           : up
#   dirty からの復旧: force <version>
#   巻き戻し        : down 1

set -e

# 拡張は毎回流す。IF NOT EXISTS なので 2 回目以降は何もしない。
# ON_ERROR_STOP=1 が無いと、失敗しても psql が 0 を返す
# (compose の db-init と同じ理由)。
for f in /init/*.sql; do
	psql -v ON_ERROR_STOP=1 -d "$DATABASE_URL" -f "$f"
done

exec migrate -path=/migrations -database="$DATABASE_URL" "$@"
