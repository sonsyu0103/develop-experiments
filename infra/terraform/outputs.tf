output "public_url" {
  description = "公開 URL。ここを開けば動く"
  value       = "https://${aws_cloudfront_distribution.main.domain_name}"
}

output "alb_dns_name" {
  description = "ALB の DNS 名。切り分けに使う (CloudFront の SG で塞いでいるので外からは叩けない)"
  value       = aws_lb.main.dns_name
}

output "ecr_repository_urls" {
  description = "イメージの push 先"
  value       = { for k, r in aws_ecr_repository.this : k => r.repository_url }
}

output "ecs_cluster_name" {
  description = "CD が更新するクラスタ"
  value       = aws_ecs_cluster.main.name
}

output "ecs_service_names" {
  description = "CD が更新するサービス"
  value = {
    api = aws_ecs_service.api.name
    web = aws_ecs_service.web.name
  }
}

output "migrate_task_definition" {
  description = "マイグレーションを流すタスク定義 (make tf-migrate が使う)"
  value       = aws_ecs_task_definition.migrate.family
}

output "github_actions_role_arn" {
  description = "GitHub Actions が OIDC で引き受けるロール。リポジトリの Variables に設定する"
  value       = aws_iam_role.github_actions.arn
}

output "db_secret_arn" {
  description = "DB の接続情報。psql でつなぐときに読む"
  value       = aws_secretsmanager_secret.db.arn
}

# **run-task に要る値をまとめて出す。**
# 手で組み立てると、サブネット ID を 1 つ書き間違えただけで
# 「タスクは起動するが ECR に届かない」という分かりにくい失敗になる。
output "run_task_network_configuration" {
  description = "aws ecs run-task に渡すネットワーク設定"
  value = {
    subnets        = aws_subnet.public[*].id
    securityGroups = [aws_security_group.ecs.id]
    assignPublicIp = "ENABLED"
  }
}

output "region" {
  description = "リソースを置いたリージョン。デプロイスクリプトが読む"
  value       = var.region
}

output "project" {
  description = "リソース名と Project タグの接頭辞。消し忘れの確認に使う"
  value       = var.project
}
