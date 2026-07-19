#!/usr/bin/env bash
# Review and destroy only resources tracked by this Terraform root module.

set -euo pipefail

# shellcheck source=scripts/aws-common.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/aws-common.sh"

require_command aws
require_command jq
require_command terraform
load_tf_var_args

terraform -chdir="${TF_DIR}" init -input=false
state_resources="$(terraform -chdir="${TF_DIR}" state list)" || die "could not read Terraform state"
if [[ -z "${state_resources}" ]]; then
  rm -f -- "${GENERATED_VARS_FILE}"
  log "Terraform state is already empty; nothing to destroy"
  exit 0
fi

identity="$(aws sts get-caller-identity --output json)"
account_id="$(jq -er '.Account' <<<"${identity}")"
principal_arn="$(jq -er '.Arn' <<<"${identity}")"
if [[ -n "${EXPECTED_AWS_ACCOUNT_ID:-}" && "${account_id}" != "${EXPECTED_AWS_ACCOUNT_ID}" ]]; then
  die "authenticated AWS account ${account_id} does not match EXPECTED_AWS_ACCOUNT_ID=${EXPECTED_AWS_ACCOUNT_ID}"
fi
region="$(tf_output region 2>/dev/null || tf_console 'var.region' | jq -er '.')"

log "AWS account: ${account_id}"
log "AWS principal: ${principal_arn}"
log "AWS region: ${region}"
log "Terraform-managed resources to destroy:"
terraform -chdir="${TF_DIR}" state list

destroy_plan="$(mktemp "${TMPDIR:-/tmp}/rpc-proxy-destroy.XXXXXX.tfplan")"
cleanup() {
  rm -f -- "${destroy_plan}"
}
trap cleanup EXIT

terraform -chdir="${TF_DIR}" plan -destroy -input=false -lock-timeout=60s \
  "${TF_VAR_ARGS[@]}" -out="${destroy_plan}"
confirm_exact "Type 'destroy ${account_id}' to apply this destruction plan: " \
  "destroy ${account_id}" "${DESTROY_AUTO_APPROVE:-0}"
terraform -chdir="${TF_DIR}" apply -input=false "${destroy_plan}"

state_resources="$(terraform -chdir="${TF_DIR}" state list)" || die "could not verify Terraform state after destroy"
if [[ -n "${state_resources}" ]]; then
  die "destroy completed but Terraform state still contains resources"
fi
rm -f -- "${GENERATED_VARS_FILE}"
log "Terraform-managed application resources were destroyed; the Route 53 hosted zone and registered domain were preserved"
