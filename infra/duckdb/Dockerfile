# S3 に着地したログを SQL で読むための使い捨てコンテナ
# (docs/adr/0010-log-pipeline.md「ローカルでの再現」)。
#
# **Athena の代替ではない。** Athena は Trino 方言で、partition projection も
# DuckDB には無い。ここで検証できるのは次の 2 つになる。
#
#   1. S3 のキー設計 (Hive 形式) が、パーティションを列として読める形か
#   2. 着地したレコードが、Athena に載せられる形に展開されているか
#
# **1 は本番の課金に直結する。** パーティションが効かないクエリは
# 毎回全期間をスキャンする (決定 2)。キーの規則性だけは手元で確かめられる。

FROM python:3.13-slim

# **バージョンを固定する。** Makefile 冒頭の方針と同じ ——
# @latest のままだと、ある日 SQL の解釈が変わって検査が突然赤くなる。
ARG DUCKDB_VERSION=1.4.1
RUN pip install --no-cache-dir "duckdb==${DUCKDB_VERSION}"

# **httpfs をイメージに焼き込む。** 実行時に取りに行かせると、
# ネットワークが無い環境で「クエリを投げた瞬間に失敗する」形になり、
# 失敗の理由がログ基盤の問題と区別できない。
RUN python -c "import duckdb; duckdb.execute('INSTALL httpfs')"

WORKDIR /work
# **PYTHONPATH に置く。** logs_source は 2 つのスクリプトが共有する。
# /usr/local/bin に混ぜると import できない (bin は sys.path に入らない)。
COPY logs_source.py /opt/logs/logs_source.py
COPY query.py /opt/logs/query.py
COPY verify.py /opt/logs/verify.py
ENV PYTHONPATH=/opt/logs

ENTRYPOINT ["python", "/opt/logs/query.py"]
