output "alb_dns_name" {
  description = "Public DNS name of the load balancer, used for DNS configuration and diagnostics."
  value       = aws_lb.this.dns_name
}

output "rpc_url" {
  description = "Canonical public URL for health, metrics, and JSON-RPC requests."
  value       = local.https_enabled ? "https://${var.public_hostname}" : "http://${aws_lb.this.dns_name}"
}

output "ecr_repository_url" {
  description = "ECR repository to push the container image to."
  value       = aws_ecr_repository.proxy.repository_url
}

output "cluster_name" {
  description = "ECS cluster name."
  value       = aws_ecs_cluster.this.name
}

output "service_name" {
  description = "ECS service name."
  value       = aws_ecs_service.proxy.name
}

output "target_group_arn" {
  description = "ALB target group ARN, used by deployment diagnostics and smoke tests."
  value       = aws_lb_target_group.this.arn
}

output "log_group_name" {
  description = "CloudWatch Logs group used by the proxy tasks."
  value       = aws_cloudwatch_log_group.proxy.name
}

output "region" {
  description = "AWS region the stack is deployed in."
  value       = var.region
}
