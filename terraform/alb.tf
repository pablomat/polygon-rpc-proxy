locals {
  https_enabled             = var.public_hostname != null && var.route53_zone_id != null
  https_configuration_valid = (var.public_hostname == null) == (var.route53_zone_id == null)
}

data "aws_route53_zone" "selected" {
  count = local.https_enabled ? 1 : 0

  zone_id      = var.route53_zone_id
  private_zone = false
}

resource "aws_acm_certificate" "this" {
  count = local.https_enabled ? 1 : 0

  domain_name       = var.public_hostname
  validation_method = "DNS"

  lifecycle {
    create_before_destroy = true

    precondition {
      condition = (
        lower(var.public_hostname) == trimsuffix(lower(data.aws_route53_zone.selected[0].name), ".") ||
        endswith(lower(var.public_hostname), ".${trimsuffix(lower(data.aws_route53_zone.selected[0].name), ".")}")
      )
      error_message = "public_hostname must be inside the public Route 53 zone selected by route53_zone_id."
    }
  }
}

resource "aws_route53_record" "certificate_validation" {
  for_each = local.https_enabled ? {
    for option in aws_acm_certificate.this[0].domain_validation_options : option.domain_name => {
      name   = option.resource_record_name
      record = option.resource_record_value
      type   = option.resource_record_type
    }
  } : {}

  zone_id = var.route53_zone_id
  name    = each.value.name
  type    = each.value.type
  ttl     = 60
  records = [each.value.record]
}

resource "aws_acm_certificate_validation" "this" {
  count = local.https_enabled ? 1 : 0

  certificate_arn         = aws_acm_certificate.this[0].arn
  validation_record_fqdns = [for record in aws_route53_record.certificate_validation : record.fqdn]

  timeouts {
    create = "15m"
  }
}

resource "aws_lb" "this" {
  name                       = var.app_name
  internal                   = false
  load_balancer_type         = "application"
  security_groups            = [aws_security_group.alb.id]
  subnets                    = aws_subnet.public[*].id
  drop_invalid_header_fields = true
}

# target_type = "ip" is required for Fargate (awsvpc networking).
resource "aws_lb_target_group" "this" {
  name        = var.app_name
  port        = var.container_port
  protocol    = "HTTP"
  vpc_id      = aws_vpc.main.id
  target_type = "ip"

  # Thirty seconds allows active requests to drain without retaining targets
  # for the 300-second AWS default.
  deregistration_delay = 30

  health_check {
    path                = "/healthz"
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.this.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type             = local.https_enabled ? "redirect" : "forward"
    target_group_arn = local.https_enabled ? null : aws_lb_target_group.this.arn

    dynamic "redirect" {
      for_each = local.https_enabled ? [1] : []

      content {
        host        = var.public_hostname
        port        = "443"
        protocol    = "HTTPS"
        status_code = "HTTP_301"
      }
    }
  }

  lifecycle {
    precondition {
      condition     = local.https_configuration_valid
      error_message = "public_hostname and route53_zone_id must be provided together."
    }
  }

  # When TLS is enabled, establish the forwarding listener before changing
  # port 80 into a redirect so there is no redirect-to-nowhere window.
  depends_on = [aws_lb_listener.https]
}

resource "aws_lb_listener" "https" {
  count = local.https_enabled ? 1 : 0

  load_balancer_arn = aws_lb.this.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = aws_acm_certificate_validation.this[0].certificate_arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.this.arn
  }

  depends_on = [aws_vpc_security_group_ingress_rule.alb_https]
}

resource "aws_route53_record" "public" {
  count = local.https_enabled ? 1 : 0

  zone_id = var.route53_zone_id
  name    = var.public_hostname
  type    = "A"

  alias {
    name                   = aws_lb.this.dns_name
    zone_id                = aws_lb.this.zone_id
    evaluate_target_health = true
  }

  # Publish the hostname only after the TLS listener is ready to serve it.
  depends_on = [aws_lb_listener.https]
}
