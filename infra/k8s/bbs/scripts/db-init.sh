#!/bin/sh
# 拡張を作る (migrate.yaml の Job migrate の initContainer)。compose の db-init と同じ手順。
#
# **先に postgres が受け付けるまで待つ。** 待たずに失敗させて
# Job のやり直しに任せると、やり直しの間隔が指数的に伸び、
# backoffLimit を使い切った時点で Job ごと Failed になる
# (postgres のイメージ取得が遅い初回に実際に起きた)。
# PGHOST=postgres なので、待ち合わせは Service 越しの TCP になる。
#
# ON_ERROR_STOP=1 が無いと、途中で失敗しても psql が 0 で終わる。
#
# このファイルは ConfigMap (job-scripts) として Job に渡す。Job の中に直接書くと、
# 書き換えても Argo CD が同期し直さない (ADR 0026「レビューで見つかったこと 6」)。
set -e
until pg_isready -q; do
	echo "postgres を待っています"
	sleep 2
done
for f in /init/*.sql; do
	psql -v ON_ERROR_STOP=1 -f "$f"
done
