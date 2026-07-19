locals {
  # Supported Linux Fargate task sizes, expressed in CPU units and MiB.
  # Keep this map in one place so invalid pairs fail during planning instead
  # of being rejected by ECS while registering the task definition.
  fargate_memory_by_cpu = {
    "256"   = [512, 1024, 2048]
    "512"   = range(1024, 4097, 1024)
    "1024"  = range(2048, 8193, 1024)
    "2048"  = range(4096, 16385, 1024)
    "4096"  = range(8192, 30721, 1024)
    "8192"  = range(16384, 61441, 4096)
    "16384" = range(32768, 122881, 8192)
    "32768" = [61440, 122880, 249856]
  }

  # ECS requires an image reference even when the service has no tasks. The
  # bootstrap reference is never pulled because a missing digest is permitted
  # only when desired_count is zero.
  container_image = var.image_digest == null ? "${aws_ecr_repository.proxy.repository_url}:bootstrap" : "${aws_ecr_repository.proxy.repository_url}@${var.image_digest}"
}

resource "aws_ecs_cluster" "this" {
  name = var.app_name
}

resource "aws_ecs_task_definition" "proxy" {
  family                   = var.app_name
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.task_cpu
  memory                   = var.task_memory
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task.arn

  lifecycle {
    # Keep the previous revision active until ECS accepts the replacement so
    # the deployment circuit breaker can roll back to it.
    create_before_destroy = true

    precondition {
      condition     = try(contains(local.fargate_memory_by_cpu[tostring(var.task_cpu)], var.task_memory), false)
      error_message = "task_cpu and task_memory must be a supported AWS Fargate CPU/memory combination."
    }
  }

  # The image is built for linux/amd64 (see scripts/push-image.sh); pin the
  # platform so the task and the pushed image can never disagree.
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "X86_64"
  }

  container_definitions = jsonencode([
    {
      name      = "proxy"
      image     = local.container_image
      essential = true
      # The distroless binary does not require root-filesystem writes.
      readonlyRootFilesystem = true
      portMappings = [
        {
          containerPort = var.container_port
          protocol      = "tcp"
        }
      ]
      environment = [
        { name = "LISTEN_ADDR", value = ":${var.container_port}" },
        { name = "UPSTREAM_URL", value = var.upstream_url },
        { name = "MAX_IN_FLIGHT", value = tostring(var.max_in_flight) },
        # Leave runtime headroom by setting the Go GC's soft memory target to
        # ~80% of the task limit.
        { name = "GOMEMLIMIT", value = "${floor(var.task_memory * 0.8)}MiB" },
      ]
      # ECS does not honor Dockerfile HEALTHCHECKs; declare the same binary
      # self-probe here so Fargate flags unhealthy containers directly, in
      # addition to the ALB target health check.
      healthCheck = {
        command     = ["CMD", "/proxy", "-healthcheck"]
        interval    = 30
        timeout     = 5
        retries     = 3
        startPeriod = 10
      }
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.proxy.name
          "awslogs-region"        = var.region
          "awslogs-stream-prefix" = "proxy"
        }
      }
    }
  ])
}

resource "aws_ecs_service" "proxy" {
  name                  = var.app_name
  cluster               = aws_ecs_cluster.this.id
  task_definition       = aws_ecs_task_definition.proxy.arn
  desired_count         = var.desired_count
  launch_type           = "FARGATE"
  wait_for_steady_state = true

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  lifecycle {
    precondition {
      condition     = var.desired_count == 0 || var.image_digest != null
      error_message = "image_digest is required when desired_count is greater than zero. Use desired_count = 0 only for the initial image bootstrap."
    }
  }

  network_configuration {
    # The default service runs in the NAT gateway's AZ to avoid cross-AZ costs.
    # This creates a single-AZ dependency; multi-AZ deployments require one NAT
    # gateway and service tasks per AZ.
    subnets          = [aws_subnet.private[0].id]
    security_groups  = [aws_security_group.task.id]
    assign_public_ip = false # private subnets; egress via the NAT gateway
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.this.arn
    container_name   = "proxy"
    container_port   = var.container_port
  }

  # A forwarding listener must exist before ECS can register tasks with the
  # target group, so order the service after all configured listeners.
  depends_on = [aws_lb_listener.http, aws_lb_listener.https]
}
