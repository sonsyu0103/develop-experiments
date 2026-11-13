#!/bin/sh
# MinIO のバケットを作る (minio.yaml の Job minio-init)。compose の minio-init と同じ手順。
#
# **手順をまるごと、通るまでやり直す** (どれも冪等)。
#
# bitnami の MinIO は起動時に、初期設定用の MinIO を裏で立てて止め、
# 本番の MinIO を立て直す (実測: 起動 0.7 秒で health が 200、5.5 秒で
# いったん応答しなくなり、その後に本番が上がる)。readinessProbe も
# 「繋がるまで待つ」も初期設定用のサーバで通ってしまい、続く mc が
# 入れ替えのすき間に当たると「Connection closed」で落ちる
# (CI で実際に落ちた)。待ち合わせで当てにいかず、やり直しで吸収する。
#
# このファイルは ConfigMap (job-scripts) として Job に渡す。Job の中に直接書くと、
# 書き換えても Argo CD が同期し直さない (ADR 0026「レビューで見つかったこと 6」)。
for i in $(seq 1 60); do
	if mc alias set local http://minio:9000 "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null 2>&1 &&
		mc mb --ignore-existing local/bbs-images &&
		mc anonymous set download local/bbs-images; then
		exit 0
	fi
	echo "minio の準備を待っています ($i)"
	sleep 2
done
echo "minio のバケットを作れなかった" >&2
exit 1
