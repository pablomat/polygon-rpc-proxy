# Security groups. Rules are standalone resources (not inline) because the
# ALB and task groups reference each other, which inline rules cannot express
# without a cycle.

# ALB: HTTP in from the internet, optional HTTPS, out only to the proxy tasks.
resource "aws_security_group" "alb" {
  name        = "${var.app_name}-alb"
  description = "Load balancer: HTTP from the internet, egress to tasks only"
  vpc_id      = aws_vpc.main.id
}

resource "aws_vpc_security_group_ingress_rule" "alb_http" {
  security_group_id = aws_security_group.alb.id
  description       = "HTTP from anywhere"
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 80
  to_port           = 80
  ip_protocol       = "tcp"
}

resource "aws_vpc_security_group_ingress_rule" "alb_https" {
  count = local.https_enabled ? 1 : 0

  security_group_id = aws_security_group.alb.id
  description       = "HTTPS from anywhere"
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 443
  to_port           = 443
  ip_protocol       = "tcp"
}

resource "aws_vpc_security_group_egress_rule" "alb_to_task" {
  security_group_id            = aws_security_group.alb.id
  description                  = "To the proxy tasks only"
  referenced_security_group_id = aws_security_group.task.id
  from_port                    = var.container_port
  to_port                      = var.container_port
  ip_protocol                  = "tcp"
}

# Tasks accept traffic only from the ALB and allow outbound TCP 443. The
# upstream RPC, ECR, S3, and CloudWatch Logs use that port through the NAT.
# Security groups do not filter traffic to the VPC DNS resolver.
resource "aws_security_group" "task" {
  name        = "${var.app_name}-task"
  description = "Proxy tasks: inbound from the ALB only, TCP 443 egress"
  vpc_id      = aws_vpc.main.id
}

resource "aws_vpc_security_group_ingress_rule" "task_from_alb" {
  security_group_id            = aws_security_group.task.id
  description                  = "From the ALB on the container port"
  referenced_security_group_id = aws_security_group.alb.id
  from_port                    = var.container_port
  to_port                      = var.container_port
  ip_protocol                  = "tcp"
}

resource "aws_vpc_security_group_egress_rule" "task_tls" {
  security_group_id = aws_security_group.task.id
  description       = "TCP 443 egress (upstream RPC, ECR, S3, CloudWatch Logs)"
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 443
  to_port           = 443
  ip_protocol       = "tcp"
}
