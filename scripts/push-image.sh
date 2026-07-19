#!/usr/bin/env bash
# Build the proxy image and push it to the ECR repository created by Terraform.
#
# Usage:
#   ./scripts/push-image.sh <ecr_repo_url> <immutable_tag> [region]
#
# Example:
#   ./scripts/push-image.sh 123456789012.dkr.ecr.eu-central-1.amazonaws.com/polygon-rpc-proxy "$(git rev-parse HEAD)" eu-central-1
#
# Requires: aws CLI (for ECR auth) and a container engine (docker or podman).
set -euo pipefail

REPO="${1:?ecr repo url required}"
TAG="${2:?immutable image tag required}"
REGION="${3:-eu-central-1}"

# Build from the repo root regardless of where the script is invoked from.
cd "$(dirname "$0")/.."

# Auto-detect a usable engine; override with CONTAINER_ENGINE=podman (or docker).
# shellcheck source=scripts/aws-common.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/aws-common.sh"
ENGINE="$(select_container_engine)"
REGISTRY="${REPO%%/*}"

[[ "${TAG}" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || die "invalid container tag: ${TAG}"

echo ">> logging in to ECR registry ${REGISTRY}"
aws ecr get-login-password --region "${REGION}" | "${ENGINE}" login --username AWS --password-stdin "${REGISTRY}"

echo ">> building ${REPO}:${TAG}"
# The ECS task runs LINUX/X86_64. Pinning the platform produces a compatible
# image on ARM64 hosts.
"${ENGINE}" build --platform linux/amd64 -t "${REPO}:${TAG}" .

echo ">> pushing ${REPO}:${TAG}"
"${ENGINE}" push "${REPO}:${TAG}"

echo ">> done"
