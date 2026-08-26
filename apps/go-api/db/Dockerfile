# マイグレーションを流すためのイメージ。
#
# **手元とは配り方が違う。**
# compose では migrate/migrate に db/migrations をボリュームで
# マウントしているが、ECS にホストのディレクトリは無い。
# マイグレーションファイルを焼き込んだイメージを作る。
#
# バージョンは compose.yaml の migrate サービスと揃える。
# ずれると「手元で通ったマイグレーションが本番で落ちる」が起きる。
FROM migrate/migrate:v4.18.1

# **拡張の作成もこのイメージでやる。**
#
# 手元では db-init サービス (postgres イメージ) が
# db/init/*.sql を流してから migrate が走る。ECS で同じことを
# サービス 2 つに分けると、**実行順序を自分で管理する**ことになる。
#
# psql を足して 1 つのタスクにまとめる。流す SQL は手元と同じファイルなので、
# 二重管理にならない。
#
# **これが無いと索引の作成で落ちる** ——
# threads_title_trgm_idx は gin_trgm_ops を使うので、
# pg_trgm が無い RDS では CREATE INDEX が失敗する (実際に踏んだ)。
RUN apk add --no-cache postgresql-client

COPY init /init
COPY migrations /migrations

# **サブコマンドを差し替えられる形にする。**
#
# `sh -c "... up"` と焼き込むと、**失敗して dirty になったときに
# force が打てない。** distroless ではないが ECS Exec も使えないので、
# イメージを作り直すまで復旧手段が無くなる (実際に踏んだ)。
#
# entrypoint.sh が引数をそのまま migrate へ渡すので、
# run-task の overrides で `force 9` や `down 1` を指定できる。
COPY entrypoint.sh /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
CMD ["up"]
