resource "aws_cloudwatch_log_group" "proxy" {
  name              = "/ecs/${var.app_name}"
  retention_in_days = var.log_retention_days
}
