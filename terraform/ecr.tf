resource "aws_ecr_repository" "proxy" {
  name                 = var.app_name
  image_tag_mutability = "IMMUTABLE"
  force_delete         = true # permits teardown with images present

  image_scanning_configuration {
    scan_on_push = true
  }
}
