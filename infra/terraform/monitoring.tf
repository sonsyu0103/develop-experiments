# 監視。
#
# **最小限にする。** 立てて壊してを繰り返す環境で、
# 通知が飛びすぎると誰も読まなくなる。
# ここで見るのは「壊れていることに気づけるか」だけになる。
#
# 通知先は任意 (alarm_email が空ならトピックだけ作って購読しない)。
# メール購読は**受信側の確認クリックが要る**ので、apply だけでは完了しない。

resource "aws_sns_topic" "alarms" {
  name = "${local.name}-alarms"
}

# **Budgets からの publish を明示的に許す。**
#
# CloudWatch アラームは SNS の既定ポリシーで publish できるが、
# **Budgets は別のサービスプリンシパルなので許可が要る。**
# 無いと通知が届かず、**消し忘れを金額で見張る仕組み (決定 1) が
# 黙って機能しない** —— この構成でいちばん怖い失敗そのものになる。
data "aws_iam_policy_document" "alarms_topic" {
  statement {
    sid    = "AllowBudgets"
    effect = "Allow"

    principals {
      type        = "Service"
      identifiers = ["budgets.amazonaws.com"]
    }

    actions   = ["SNS:Publish"]
    resources = [aws_sns_topic.alarms.arn]

    # 他人のアカウントの予算から publish されないよう絞る。
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }

  statement {
    sid    = "AllowCloudWatchAlarms"
    effect = "Allow"

    principals {
      type        = "Service"
      identifiers = ["cloudwatch.amazonaws.com"]
    }

    actions   = ["SNS:Publish"]
    resources = [aws_sns_topic.alarms.arn]

    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

resource "aws_sns_topic_policy" "alarms" {
  arn    = aws_sns_topic.alarms.arn
  policy = data.aws_iam_policy_document.alarms_topic.json
}

resource "aws_sns_topic_subscription" "alarms_email" {
  count = var.alarm_email == "" ? 0 : 1

  topic_arn = aws_sns_topic.alarms.arn
  protocol  = "email"
  endpoint  = var.alarm_email
}

# **ALB が 5xx を返している。**
# アプリが落ちているか、ターゲットが居ない状態。
# 手元の https-verify が「書き込みだけ 403」を捕まえるのと違い、
# ここが見るのは「そもそも応答できていない」ほう。
resource "aws_cloudwatch_metric_alarm" "alb_5xx" {
  alarm_name          = "${local.name}-alb-5xx"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  threshold           = 5
  period              = 300
  statistic           = "Sum"

  namespace   = "AWS/ApplicationELB"
  metric_name = "HTTPCode_ELB_5XX_Count"

  dimensions = {
    LoadBalancer = aws_lb.main.arn_suffix
  }

  alarm_description  = "ALB が 5xx を返している (ターゲットが居ないか、応答していない)"
  alarm_actions      = [aws_sns_topic.alarms.arn]
  treat_missing_data = "notBreaching"
}

# **健全なターゲットが 0 になった。**
# Spot の中断や、ヘルスチェック失敗による入れ替えが続いている状態。
resource "aws_cloudwatch_metric_alarm" "api_unhealthy" {
  alarm_name          = "${local.name}-api-no-healthy-target"
  comparison_operator = "LessThanThreshold"
  evaluation_periods  = 2
  threshold           = 1
  period              = 60
  statistic           = "Minimum"

  namespace   = "AWS/ApplicationELB"
  metric_name = "HealthyHostCount"

  dimensions = {
    LoadBalancer = aws_lb.main.arn_suffix
    TargetGroup  = aws_lb_target_group.api.arn_suffix
  }

  alarm_description  = "go-api の健全なターゲットが居ない"
  alarm_actions      = [aws_sns_topic.alarms.arn]
  treat_missing_data = "breaching"
}

# **DB の CPU。**
# db.t4g.micro はバーストするので、張り付いたら構成の見直しが要る
# (ADR 0009 のスケール戦略)。
resource "aws_cloudwatch_metric_alarm" "rds_cpu" {
  alarm_name          = "${local.name}-rds-cpu"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  threshold           = 80
  period              = 300
  statistic           = "Average"

  namespace   = "AWS/RDS"
  metric_name = "CPUUtilization"

  dimensions = {
    DBInstanceIdentifier = aws_db_instance.main.identifier
  }

  alarm_description  = "RDS の CPU が張り付いている"
  alarm_actions      = [aws_sns_topic.alarms.arn]
  treat_missing_data = "notBreaching"
}

# **消し忘れの見張り。**
# 「使う日だけ立てる」運用でいちばん怖いのは、destroy を忘れて
# 月末に請求が来ること。**予算アラートは無料**なので必ず置く。
resource "aws_budgets_budget" "monthly" {
  name         = "${local.name}-monthly"
  budget_type  = "COST"
  limit_amount = var.monthly_budget_usd
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  # 実績が閾値を超えたら通知。
  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 80
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_sns_topic_arns  = [aws_sns_topic.alarms.arn]
    subscriber_email_addresses = var.alarm_email == "" ? [] : [var.alarm_email]
  }

  # **予測でも通知する。** 実績が閾値に達したときには
  # 既に使ってしまっている。予測のほうが止められる。
  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "FORECASTED"
    subscriber_sns_topic_arns  = [aws_sns_topic.alarms.arn]
    subscriber_email_addresses = var.alarm_email == "" ? [] : [var.alarm_email]
  }
}
