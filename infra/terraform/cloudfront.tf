# CloudFront。手元の Caddy (infra/caddy/Caddyfile) と同じ役割を持つ。
#
# ── **独自ドメインを取らない** ─────────────────────────────
#
# CloudFront の既定ドメイン (*.cloudfront.net) には**証明書が付いてくる**ので、
# ドメイン代 (年 $13) と Route 53 のホストゾーン (月 $0.50) を払わずに
# HTTPS で公開できる。
#
# 代償は 2 つ:
#   - URL が覚えにくい (d1234abcd.cloudfront.net)
#   - CloudFront ←→ ALB 間が HTTP になる (ALB に証明書を付けられないため)
#
# 独自ドメインに移すときは、ACM (us-east-1) で証明書を取り、
# aliases と viewer_certificate を差し替え、ALB のリスナーを 443 にする。
# そこまでやると X-Forwarded-Proto も https になる (alb.tf の注記)。

# **/api/* のプレフィックスを剥がす。**
# 手元では Caddy の handle_path がやっていた仕事。
# ALB はパスを書き換えられないので、ここで剥がす。
resource "aws_cloudfront_function" "strip_api_prefix" {
  name    = "${local.name}-strip-api-prefix"
  runtime = "cloudfront-js-2.0"
  publish = true

  comment = "/api/* のプレフィックスを剥がして go-api へ渡す"

  code = <<-JS
    function handler(event) {
      var request = event.request;

      // ビヘイビアの選択はこの関数より前に終わっているので、
      // ここで書き換えてもオリジンの振り分けには影響しない。
      request.uri = request.uri.replace(/^\/api/, '');

      // /api だけで来た場合に空文字にしない。
      if (request.uri === '') {
        request.uri = '/';
      }
      return request;
    }
  JS
}

# S3 を公開せずに CloudFront から読ませる。
resource "aws_cloudfront_origin_access_control" "images" {
  name                              = "${local.name}-images"
  origin_access_control_origin_type = "s3"
  signing_behavior                  = "always"
  signing_protocol                  = "sigv4"
}

# セキュリティヘッダ。**手元の Caddy と同じものを付ける** (ADR 0013 決定 2)。
# 責務の置き場所も揃う —— どちらもエッジで付けている。
resource "aws_cloudfront_response_headers_policy" "security" {
  name = "${local.name}-security-headers"

  security_headers_config {
    content_type_options {
      override = true
    }

    frame_options {
      frame_option = "DENY"
      override     = true
    }

    referrer_policy {
      referrer_policy = "strict-origin-when-cross-origin"
      override        = true
    }

    # **ここでは HSTS を有効にする。**
    # 手元 (localhost) では max-age=0 にしていた —— HSTS はポートを
    # 区別しないので、http://localhost:3000 での開発まで巻き添えになるため
    # (ADR 0023 の 4)。**本番ホストにはその問題が無い。**
    strict_transport_security {
      access_control_max_age_sec = 31536000
      # *.cloudfront.net のサブドメインを巻き込まないよう false。
      include_subdomains = false
      preload            = false
      override           = true
    }
  }
}

locals {
  origin_alb_web = "alb-web"
  origin_alb_api = "alb-api"
  origin_images  = "s3-images"

  # マネージドポリシーの ID (AWS が固定で払い出しているもの)。
  cache_disabled  = "4135ea2d-6df8-44a3-9df3-4b5a84be39ad"
  cache_optimized = "658327ea-f89d-4fab-a63d-7e88639e58f6"

  # **AllViewer を使う。** Host ヘッダを含めて全部オリジンへ渡す。
  #
  # AllViewerExceptHostHeader にすると、ALB へ届く Host が
  # ALB の DNS 名になる。go-api の selfOrigin() は Host から
  # オリジンを組み立てるので (middleware.go)、
  # **CORS の許可リストと一致しなくなり、書き込みが 403 になる。**
  origin_request_all_viewer = "216adef6-5c7f-47e4-b989-5492eafa07d3"
}

resource "aws_cloudfront_distribution" "main" {
  enabled = true
  comment = local.name

  # **アジアを含む価格クラス。** PriceClass_100 は北米と欧州だけで、
  # 日本から見ると目に見えて遅くなる。
  price_class = "PriceClass_200"

  # --- オリジン ---------------------------------------------------------

  # 画面 (next-app)。ALB の 80 番。
  origin {
    origin_id   = local.origin_alb_web
    domain_name = aws_lb.main.dns_name

    custom_origin_config {
      http_port              = 80
      https_port             = 443
      origin_protocol_policy = "http-only"
      origin_ssl_protocols   = ["TLSv1.2"]
    }
  }

  # API (go-api)。**同じ ALB の 8080 番。**
  # ALB がパスを書き換えられないので、ポートで経路を分けている (alb.tf)。
  origin {
    origin_id   = local.origin_alb_api
    domain_name = aws_lb.main.dns_name

    custom_origin_config {
      http_port              = 8080
      https_port             = 443
      origin_protocol_policy = "http-only"
      origin_ssl_protocols   = ["TLSv1.2"]
    }
  }

  # 画像。S3 を直接読む (API を通さない)。
  origin {
    origin_id                = local.origin_images
    domain_name              = aws_s3_bucket.images.bucket_regional_domain_name
    origin_access_control_id = aws_cloudfront_origin_access_control.images.id
  }

  # --- ビヘイビア -------------------------------------------------------

  # 既定は画面へ。
  default_cache_behavior {
    target_origin_id       = local.origin_alb_web
    viewer_protocol_policy = "redirect-to-https"

    allowed_methods = ["GET", "HEAD", "OPTIONS"]
    cached_methods  = ["GET", "HEAD"]

    # **キャッシュしない。** Next.js の App Router は
    # Server Components で毎回組み立てる。ここでキャッシュすると
    # ログイン状態が他人に見える事故になる。
    cache_policy_id            = local.cache_disabled
    origin_request_policy_id   = local.origin_request_all_viewer
    response_headers_policy_id = aws_cloudfront_response_headers_policy.security.id
  }

  # API。**書き込みメソッドを通す。**
  ordered_cache_behavior {
    path_pattern           = "/api/*"
    target_origin_id       = local.origin_alb_api
    viewer_protocol_policy = "redirect-to-https"

    allowed_methods = ["GET", "HEAD", "OPTIONS", "PUT", "POST", "PATCH", "DELETE"]
    cached_methods  = ["GET", "HEAD"]

    cache_policy_id            = local.cache_disabled
    origin_request_policy_id   = local.origin_request_all_viewer
    response_headers_policy_id = aws_cloudfront_response_headers_policy.security.id

    function_association {
      event_type   = "viewer-request"
      function_arn = aws_cloudfront_function.strip_api_prefix.arn
    }
  }

  # 画像。**ここだけキャッシュする。**
  ordered_cache_behavior {
    path_pattern           = "/images/*"
    target_origin_id       = local.origin_images
    viewer_protocol_policy = "redirect-to-https"

    allowed_methods = ["GET", "HEAD"]
    cached_methods  = ["GET", "HEAD"]

    cache_policy_id            = local.cache_optimized
    response_headers_policy_id = aws_cloudfront_response_headers_policy.security.id
  }

  restrictions {
    geo_restriction {
      restriction_type = "none"
    }
  }

  # 既定ドメインの証明書を使う (独自ドメインを取らない選択の帰結)。
  viewer_certificate {
    cloudfront_default_certificate = true
  }

  tags = { Name = local.name }
}
