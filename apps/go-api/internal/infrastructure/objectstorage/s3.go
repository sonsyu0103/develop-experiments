// Package objectstorage は S3 互換ストレージへの実装です。
//
// **S3 SDK の型がこのパッケージの外へ出ません**
// (docs/adr/0007-image-storage.md のモジュール構成)。
// image モジュールは repository.ObjectStorage しか知りません。
//
// ローカルは MinIO、本番は S3 + CloudFront を想定します。
// 差はエンドポイントとパス形式の設定だけに閉じます。
package objectstorage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"develop-experiments/apps/go-api/internal/config"
	"develop-experiments/apps/go-api/internal/image/domain/repository"
)

// S3Storage は repository.ObjectStorage の S3 互換実装です。
type S3Storage struct {
	client *s3.Client
	bucket string
	// publicBaseURL は配信 URL の前置きです (末尾のスラッシュを含みません)。
	publicBaseURL string
}

var _ repository.ObjectStorage = (*S3Storage)(nil)

// New はストレージのクライアントを組み立てます。
//
// 【資格情報の解決を 2 通り持つ理由】
// MinIO は静的なキーで喋りますが、本番の ECS / EKS では
// **タスクロールで解決させたい** (キーを環境変数に置かない)。
// 明示的なキーが設定されている場合だけ静的に固定し、
// 無ければ SDK の既定チェーンに任せます。
func New(ctx context.Context, cfg config.StorageConfig) (*S3Storage, error) {
	if !cfg.Enabled() {
		return nil, fmt.Errorf("objectstorage: 設定が揃っていません (bucket / public_base_url が必要です)")
	}

	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("objectstorage: AWS 設定の読み込みに失敗しました: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		// **MinIO では path-style が必要になります。**
		// 既定の virtual-hosted style は bucket.host という名前解決を要求し、
		// localhost では解決できません (bucket.localhost は引けない)。
		o.UsePathStyle = cfg.UsePathStyle
	})

	return &S3Storage{
		client:        client,
		bucket:        cfg.Bucket,
		publicBaseURL: strings.TrimRight(cfg.PublicBaseURL, "/"),
	}, nil
}

// Put はオブジェクトを書き込みます。
//
// **ACL を設定しません。** バケットを直接公開しない設計であり
// (ADR 0007 決定 4)、公開は前段の CloudFront が OAC 経由で行います。
// ここで public-read を付けると、CloudFront を迂回して原本を叩ける経路が
// できてしまい、キャッシュもレート制限も効かなくなります。
func (s *S3Storage) Put(ctx context.Context, key, contentType string, body []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(contentType),
		// 長さを明示する。bytes.Reader は Seek できるので SDK 側でも
		// 求められますが、明示しておくと chunked 送信に落ちません。
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		return fmt.Errorf("objectstorage: PUT に失敗しました (key=%s): %w", key, err)
	}
	return nil
}

// Delete はオブジェクトを消します。
//
// **存在しないキーを成功として扱います。**
// 回収バッチは「S3 は消したが DB 行の更新前に落ちた」状態から
// 再実行されるため、ここで失敗にすると回収が永久に進まなくなります
// (ADR 0007 決定 3 の孤児回収)。
//
// なお S3 の DeleteObject はもともと存在しないキーでも成功を返しますが、
// **MinIO を含む互換実装がそうである保証はありません**。
// NoSuchKey を明示的に吸収しておきます。
func (s *S3Storage) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return nil
	}

	var notFound *types.NoSuchKey
	if errors.As(err, &notFound) {
		return nil
	}
	return fmt.Errorf("objectstorage: DELETE に失敗しました (key=%s): %w", key, err)
}

// URL は配信用の絶対 URL を返します (ADR 0007 決定 5)。
//
// キーは UUID とこちらが決めた拡張子だけで構成されるため
// (ADR 0007 決定 3)、エスケープが要る文字は現れません。
// それでも PathEscape を通すのは、**キーの作り方が変わったときに
// URL が壊れるのを防ぐ**ためです。スラッシュは区切りとして残します。
func (s *S3Storage) URL(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return s.publicBaseURL + "/" + strings.Join(parts, "/")
}
