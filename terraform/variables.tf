variable "app_name" {
  description = "Name prefix for all resources."
  type        = string
  default     = "polygon-rpc-proxy"

  validation {
    condition = (
      length(var.app_name) >= 2 &&
      length(var.app_name) <= 32 &&
      can(regex("^[a-z0-9]([a-z0-9-]*[a-z0-9])$", var.app_name)) &&
      !startswith(var.app_name, "internal-")
    )
    error_message = "app_name must be 2-32 lowercase letters, digits, or hyphens; start and end with a letter or digit; and not start with internal-."
  }
}

variable "region" {
  description = "AWS region to deploy into."
  type        = string
  default     = "eu-central-1"
}

variable "public_hostname" {
  description = "Public DNS hostname for optional Terraform-managed HTTPS. Set with route53_zone_id."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition = var.public_hostname == null ? true : (
      length(var.public_hostname) <= 253 &&
      can(regex("^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\\.)+[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$", var.public_hostname))
    )
    error_message = "public_hostname must be a DNS hostname without a scheme, port, path, or trailing dot."
  }
}

variable "route53_zone_id" {
  description = "ID of the existing public Route 53 hosted zone for public_hostname. Set with public_hostname."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.route53_zone_id == null ? true : can(regex("^Z[A-Z0-9]+$", var.route53_zone_id))
    error_message = "route53_zone_id must be a Route 53 hosted zone ID such as Z0123456789ABC."
  }
}

variable "vpc_cidr" {
  description = "CIDR block for the dedicated VPC."
  type        = string
  default     = "10.0.0.0/16"
}

variable "image_digest" {
  description = "Immutable ECR image digest to deploy. Leave null only while bootstrapping with desired_count = 0."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.image_digest == null ? true : can(regex("^sha256:[0-9a-f]{64}$", var.image_digest))
    error_message = "image_digest must use the form sha256:<64 lowercase hexadecimal characters>."
  }
}

variable "container_port" {
  description = "Port the proxy listens on inside the container."
  type        = number
  default     = 8080

  validation {
    condition     = var.container_port >= 1 && var.container_port <= 65535 && floor(var.container_port) == var.container_port
    error_message = "container_port must be an integer between 1 and 65535."
  }
}

variable "desired_count" {
  description = "Number of Fargate tasks to run."
  type        = number
  default     = 1

  validation {
    condition     = var.desired_count >= 0 && floor(var.desired_count) == var.desired_count
    error_message = "desired_count must be a non-negative integer. Use zero only for image bootstrapping."
  }
}

variable "max_in_flight" {
  description = "Maximum number of RPC requests admitted concurrently by each proxy task."
  type        = number
  default     = 128

  validation {
    condition     = var.max_in_flight >= 1 && floor(var.max_in_flight) == var.max_in_flight
    error_message = "max_in_flight must be a positive integer."
  }
}

variable "task_cpu" {
  description = "Fargate task CPU units (256 = 0.25 vCPU)."
  type        = number
  default     = 256

  validation {
    condition     = contains([256, 512, 1024, 2048, 4096, 8192, 16384, 32768], var.task_cpu)
    error_message = "task_cpu must be a CPU size supported by AWS Fargate."
  }
}

variable "task_memory" {
  description = "Fargate task memory in MiB."
  type        = number
  default     = 512

  validation {
    condition     = var.task_memory > 0 && floor(var.task_memory) == var.task_memory
    error_message = "task_memory must be a positive integer; its compatibility with task_cpu is validated on the task definition."
  }
}

variable "upstream_url" {
  description = "HTTPS upstream RPC endpoint reachable on TCP port 443."
  type        = string
  default     = "https://polygon.drpc.org"

  validation {
    condition     = can(regex("^https://([^/?#:@[:space:]]+|\\[[0-9A-Fa-f:]+\\])(:443)?([/?#].*)?$", var.upstream_url))
    error_message = "upstream_url must be an HTTPS URL with a non-empty host and no port other than 443."
  }
}

variable "log_retention_days" {
  description = "CloudWatch log retention in days."
  type        = number
  default     = 7

  validation {
    condition     = contains([1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653], var.log_retention_days)
    error_message = "log_retention_days must be a retention period supported by CloudWatch Logs."
  }
}
