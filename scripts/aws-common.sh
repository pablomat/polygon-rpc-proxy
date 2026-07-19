#!/usr/bin/env bash
# Shared helpers for the local AWS deployment workflow.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
TF_DIR="${REPO_ROOT}/terraform"
# Used by deploy.sh and destroy.sh after they source this helper.
# shellcheck disable=SC2034
GENERATED_VARS_FILE="${TF_DIR}/generated.auto.tfvars.json"
TF_VAR_ARGS=()

export AWS_PAGER=""
export TF_IN_AUTOMATION=1

log() {
  printf '>> %s\n' "$*"
}

warn() {
  printf 'warning: %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

load_tf_var_args() {
  local vars_file="${TF_VARS_FILE:-}"

  if [[ -z "${vars_file}" && -f "${TF_DIR}/deployment.tfvars" ]]; then
    vars_file="${TF_DIR}/deployment.tfvars"
  elif [[ -n "${vars_file}" && "${vars_file}" != /* ]]; then
    vars_file="${REPO_ROOT}/${vars_file}"
  fi

  if [[ -n "${vars_file}" ]]; then
    [[ -f "${vars_file}" ]] || die "Terraform variable file not found: ${vars_file}"
    TF_VAR_ARGS=("-var-file=${vars_file}")
    log "Terraform configuration: ${vars_file}"
  else
    TF_VAR_ARGS=()
    log "Terraform configuration: HTTP defaults"
  fi
}

tf_output() {
  terraform -chdir="${TF_DIR}" output -raw "$1"
}

tf_console() {
  printf '%s\n' "$1" | terraform -chdir="${TF_DIR}" console -no-color "${TF_VAR_ARGS[@]}"
}

confirm_exact() {
  local prompt="$1"
  local expected="$2"
  local approved="${3:-0}"
  local answer

  if [[ "${approved}" == "1" ]]; then
    log "Confirmation supplied by environment"
    return
  fi
  [[ -t 0 ]] || die "interactive confirmation is required; use the documented approval environment variable in automation"
  read -r -p "${prompt}" answer
  [[ "${answer}" == "${expected}" ]] || die "confirmation did not match; no changes were applied"
}

select_container_engine() {
  local candidate
  local candidates=()

  if [[ -n "${CONTAINER_ENGINE:-}" ]]; then
    candidates=("${CONTAINER_ENGINE}")
  else
    candidates=(docker podman)
  fi

  for candidate in "${candidates[@]}"; do
    command -v "${candidate}" >/dev/null 2>&1 || continue
    if "${candidate}" info >/dev/null 2>&1; then
      printf '%s\n' "${candidate}"
      return
    fi
    warn "${candidate} is installed but is not usable"
  done

  die "a working Docker or Podman engine is required; set CONTAINER_ENGINE to choose one explicitly"
}

deployment_diagnostics() {
  local region cluster service target_group log_group stopped

  region="$(tf_output region 2>/dev/null || true)"
  cluster="$(tf_output cluster_name 2>/dev/null || true)"
  service="$(tf_output service_name 2>/dev/null || true)"
  target_group="$(tf_output target_group_arn 2>/dev/null || true)"
  log_group="$(tf_output log_group_name 2>/dev/null || true)"

  [[ -n "${region}" ]] || return 0
  warn "deployment failed; collecting AWS diagnostics"

  if [[ -n "${cluster}" && -n "${service}" ]]; then
    aws ecs describe-services --region "${region}" --cluster "${cluster}" --services "${service}" \
      --query 'services[0].{status:status,desired:desiredCount,running:runningCount,pending:pendingCount,deployments:deployments,events:events[0:10]}' \
      --output json || true
    stopped="$(aws ecs list-tasks --region "${region}" --cluster "${cluster}" --service-name "${service}" \
      --desired-status STOPPED --max-items 10 --query 'taskArns' --output text 2>/dev/null || true)"
    if [[ -n "${stopped}" && "${stopped}" != "None" ]]; then
      # AWS CLI accepts the tab-separated text output as separate words here.
      # shellcheck disable=SC2086
      aws ecs describe-tasks --region "${region}" --cluster "${cluster}" --tasks ${stopped} \
        --query 'tasks[].{task:taskArn,stopCode:stopCode,reason:stoppedReason,containers:containers[].{name:name,reason:reason,exitCode:exitCode}}' \
        --output json || true
    fi
  fi
  if [[ -n "${target_group}" ]]; then
    aws elbv2 describe-target-health --region "${region}" --target-group-arn "${target_group}" --output json || true
  fi
  if [[ -n "${log_group}" ]]; then
    aws logs tail "${log_group}" --region "${region}" --since 15m --format short 2>/dev/null || true
  fi
}
