# 画像の置き場 (ADR 0007)。
#
# 手元では MinIO が同じ役割をしている。
# **バケットを公開しない** —— CloudFront から OAC で読ませる。
# 公開バケットにすると、CloudFront を迂回して直接叩かれる経路ができ、
# 転送量の課金も CloudFront のキャッシュも効かなくなる。

resource "aws_s3_bucket" "images" {
  # **バケット名はグローバルに一意**でなければならない。
  # アカウント ID を混ぜて衝突を避ける。
  bucket = "${local.name}-images-${data.aws_caller_identity.current.account_id}"

  # **中身が残っていても destroy できるようにする。**
  # これが false だと、画像を 1 枚でも投稿した時点で
  # terraform destroy が失敗する。
  force_destroy = true
}

data "aws_caller_identity" "current" {}

resource "aws_s3_bucket_public_access_block" "images" {
  bucket = aws_s3_bucket.images.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# **暗号化はしておく。** S3 の既定は SSE-S3 で暗号化されるが、
# 明示的に書くことで「意図して選んだ」ことが構成に残る。
resource "aws_s3_bucket_server_side_encryption_configuration" "images" {
  bucket = aws_s3_bucket.images.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# CloudFront (OAC) だけに読ませる。
data "aws_iam_policy_document" "images_bucket" {
  statement {
    sid    = "AllowCloudFrontServicePrincipalReadOnly"
    effect = "Allow"

    principals {
      type        = "Service"
      identifiers = ["cloudfront.amazonaws.com"]
    }

    actions   = ["s3:GetObject"]
    resources = ["${aws_s3_bucket.images.arn}/*"]

    # **このディストリビューションからのみ。**
    # 条件を付けないと、他人の CloudFront からも読めてしまう。
    condition {
      test     = "StringEquals"
      variable = "AWS:SourceArn"
      values   = [aws_cloudfront_distribution.main.arn]
    }
  }
}

resource "aws_s3_bucket_policy" "images" {
  bucket = aws_s3_bucket.images.id
  policy = data.aws_iam_policy_document.images_bucket.json
}
