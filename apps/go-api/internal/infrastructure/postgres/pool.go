// Package postgres は PostgreSQL への接続と、リポジトリの実装を提供します。
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/config"
)

// NewPool は pgxpool の接続プールを生成し、疎通確認まで済ませて返します。
//
// database/sql を経由せず pgx ネイティブのインターフェースを使う理由:
//   - database/sql の interface{} 経由の値変換を挟まないぶん、
//     バイナリプロトコルをそのまま扱えてアロケーションが減る
//   - COPY プロトコル、LISTEN/NOTIFY、配列型など PostgreSQL 固有機能を使える
//   - sqlc が pgx/v5 向けのコードを直接生成できる
func NewPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: DATABASE_URL の解析に失敗しました: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns

	// 接続を無期限に使い回すと、DB 側のフェイルオーバーや
	// 長時間接続に伴うメモリ肥大を拾えなくなるため上限を設ける。
	poolCfg.MaxConnLifetime = time.Hour
	// ライフタイム満了が同時に集中しないよう、最大 5 分のゆらぎを入れる。
	poolCfg.MaxConnLifetimeJitter = 5 * time.Minute
	poolCfg.MaxConnIdleTime = 30 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: 接続プールの生成に失敗しました: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: 疎通確認に失敗しました: %w", err)
	}

	return pool, nil
}
