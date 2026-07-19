#!/usr/bin/env bash
# Build and deploy the current commit to ECS, then run the smoke suite.

set -euo pipefail

# shellcheck source=scripts/aws-common.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/aws-common.sh"

require_command aws
require_command curl
require_command date
require_command git
require_command jq
require_command terraform
load_tf_var_args

cd "${REPO_ROOT}"
[[ "$(git rev-parse --show-toplevel)" == "${REPO_ROOT}" ]] || die "run this workflow from the repository checkout"
dirty_tree="$(git status --porcelain --untracked-files=normal)"
if [[ -n "${dirty_tree}" && "${DEPLOY_ALLOW_DIRTY:-0}" != "1" ]]; then
  die "the Git tree is dirty; commit the exact artifact being deployed or set DEPLOY_ALLOW_DIRTY=1 for an explicit local override"
fi

commit_sha="$(git rev-parse HEAD)"
image_tag="${commit_sha}"
if [[ -n "${dirty_tree}" ]]; then
  image_tag="${commit_sha}-dirty-$(date -u +%Y%m%d%H%M%S)"
  warn "deploying an uncommitted diagnostic image as ${image_tag}"
fi
engine="$(select_container_engine)"
identity="$(aws sts get-caller-identity --output json)"
account_id="$(jq -er '.Account' <<<"${identity}")"
principal_arn="$(jq -er '.Arn' <<<"${identity}")"
if [[ -n "${EXPECTED_AWS_ACCOUNT_ID:-}" && "${account_id}" != "${EXPECTED_AWS_ACCOUNT_ID}" ]]; then
  die "authenticated AWS account ${account_id} does not match EXPECTED_AWS_ACCOUNT_ID=${EXPECTED_AWS_ACCOUNT_ID}"
fi

terraform -chdir="${TF_DIR}" init -input=false
terraform -chdir="${TF_DIR}" validate

region="$(tf_console 'var.region' | jq -er '.')"
hostname="$(tf_console 'var.public_hostname == null ? "" : var.public_hostname' | jq -er '.')"
desired_count="$(tf_console 'var.desired_count' | jq -er '.')"
[[ "${desired_count}" =~ ^[1-9][0-9]*$ ]] || die "the final desired count must be a positive integer"

log "AWS account: ${account_id}"
log "AWS principal: ${principal_arn}"
log "AWS region: ${region}"
log "Git commit: ${commit_sha}"
log "Image tag: ${image_tag}"
log "Container engine: ${engine}"
if [[ -n "${hostname}" ]]; then
  log "TLS hostname: ${hostname}"
fi

bootstrap_vars="$(mktemp "${TMPDIR:-/tmp}/rpc-proxy-bootstrap.XXXXXX.tfvars.json")"
bootstrap_plan="$(mktemp "${TMPDIR:-/tmp}/rpc-proxy-bootstrap.XXXXXX.tfplan")"
release_plan="$(mktemp "${TMPDIR:-/tmp}/rpc-proxy-release.XXXXXX.tfplan")"
generated_tmp="${GENERATED_VARS_FILE}.tmp"
cleanup() {
  rm -f -- "${bootstrap_vars}" "${bootstrap_plan}" "${release_plan}" "${generated_tmp}"
}
trap cleanup EXIT
jq -n '{desired_count: 0, image_digest: null}' >"${bootstrap_vars}"

publish_approved=0
if ! terraform -chdir="${TF_DIR}" state show aws_ecr_repository.proxy >/dev/null 2>&1; then
  log "Planning the zero-task infrastructure bootstrap"
  terraform -chdir="${TF_DIR}" plan -input=false -lock-timeout=60s \
    "${TF_VAR_ARGS[@]}" -var-file="${bootstrap_vars}" -out="${bootstrap_plan}"
  confirm_exact "Type 'apply' to create the bootstrap infrastructure and authorize image publication in account ${account_id}: " \
    apply "${TF_AUTO_APPROVE:-0}"
  if ! terraform -chdir="${TF_DIR}" apply -input=false "${bootstrap_plan}"; then
    deployment_diagnostics
    die "Terraform bootstrap failed"
  fi
  publish_approved=1
fi

repository_url="$(tf_output ecr_repository_url)"
region="$(tf_output region)"
repository_name="${repository_url#*/}"

digest="$(aws ecr describe-images --region "${region}" --repository-name "${repository_name}" \
  --image-ids "imageTag=${image_tag}" --query 'imageDetails[0].imageDigest' --output text 2>/dev/null || true)"
if [[ -n "${digest}" && "${digest}" != "None" ]]; then
  log "Reusing immutable image ${repository_url}:${image_tag}"
else
  if [[ "${publish_approved}" != "1" ]]; then
    confirm_exact "Type 'publish ${image_tag}' to push the image to account ${account_id}: " \
      "publish ${image_tag}" "${TF_AUTO_APPROVE:-0}"
  fi
  CONTAINER_ENGINE="${engine}" "${SCRIPT_DIR}/push-image.sh" "${repository_url}" "${image_tag}" "${region}"
  digest="$(aws ecr describe-images --region "${region}" --repository-name "${repository_name}" \
    --image-ids "imageTag=${image_tag}" --query 'imageDetails[0].imageDigest' --output text)"
fi
[[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "ECR returned an invalid image digest: ${digest}"

jq -n --arg digest "${digest}" --argjson desired_count "${desired_count}" \
  '{image_digest: $digest, desired_count: $desired_count}' >"${generated_tmp}"
mv -- "${generated_tmp}" "${GENERATED_VARS_FILE}"
log "Pinned deployment to ${repository_url}@${digest}"

log "Planning the release"
terraform -chdir="${TF_DIR}" plan -input=false -lock-timeout=60s "${TF_VAR_ARGS[@]}" \
  -var="image_digest=${digest}" -var="desired_count=${desired_count}" -out="${release_plan}"
confirm_exact "Type 'apply' to deploy commit ${commit_sha} in account ${account_id}: " \
  apply "${TF_AUTO_APPROVE:-0}"
if ! terraform -chdir="${TF_DIR}" apply -input=false "${release_plan}"; then
  deployment_diagnostics
  die "Terraform release failed or the ECS deployment circuit breaker rolled it back"
fi

cluster="$(tf_output cluster_name)"
service="$(tf_output service_name)"
target_group="$(tf_output target_group_arn)"
if ! aws ecs wait services-stable --region "${region}" --cluster "${cluster}" --services "${service}"; then
  deployment_diagnostics
  die "ECS did not reach a stable state"
fi
if ! aws elbv2 wait target-in-service --region "${region}" --target-group-arn "${target_group}"; then
  deployment_diagnostics
  die "the ALB target did not become healthy"
fi

if ! "${SCRIPT_DIR}/smoke-test.sh"; then
  deployment_diagnostics
  die "deployment completed, but smoke tests failed"
fi

log "Deployment complete: $(tf_output rpc_url)"
