#!/usr/bin/env bash
# End-to-end checks that do not change AWS infrastructure.

set -euo pipefail

# shellcheck source=scripts/aws-common.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/aws-common.sh"

require_command aws
require_command curl
require_command dd
require_command jq
require_command terraform
load_tf_var_args

rpc_url="$(tf_output rpc_url)"
alb_dns="$(tf_output alb_dns_name)"
region="$(tf_output region)"
cluster="$(tf_output cluster_name)"
service="$(tf_output service_name)"
target_group="$(tf_output target_group_arn)"
log_group="$(tf_output log_group_name)"

case "${rpc_url}" in
  https://*) scheme=https; port=443; host="${rpc_url#https://}" ;;
  http://*) scheme=http; port=80; host="${rpc_url#http://}" ;;
  *) die "unsupported rpc_url output: ${rpc_url}" ;;
esac
[[ "${host}" != */* ]] || die "rpc_url must not contain a path"

curl_common=(--connect-timeout 10 --max-time 30 --retry 3 --retry-delay 2 --retry-max-time 45 --retry-connrefused --silent --show-error)
if [[ -n "${SMOKE_RESOLVE_IP:-}" ]]; then
  [[ "${SMOKE_RESOLVE_IP}" =~ ^[0-9A-Fa-f:.]+$ ]] || die "SMOKE_RESOLVE_IP must be a literal IPv4 or IPv6 address"
  curl_common+=(--resolve "${host}:${port}:${SMOKE_RESOLVE_IP}")
  log "Using explicit curl resolution override ${host}:${port}:${SMOKE_RESOLVE_IP}; system DNS is not being tested"
fi

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/rpc-proxy-smoke.XXXXXX")"
cleanup() {
  rm -f -- "${tmp_dir}/body" "${tmp_dir}/headers" "${tmp_dir}/oversized"
  rmdir -- "${tmp_dir}"
}
trap cleanup EXIT

expect_status() {
  local expected="$1"
  shift
  local actual
  actual="$(curl "${curl_common[@]}" -o "${tmp_dir}/body" -w '%{http_code}' "$@")"
  [[ "${actual}" == "${expected}" ]] || die "expected HTTP ${expected}, received ${actual} for $*"
}

log "Waiting for ECS and ALB health"
aws ecs wait services-stable --region "${region}" --cluster "${cluster}" --services "${service}"
aws elbv2 wait target-in-service --region "${region}" --target-group-arn "${target_group}"

log "Checking health and JSON-RPC forwarding at ${rpc_url}"
expect_status 200 "${rpc_url}/healthz"
jq -e '.status == "ok"' "${tmp_dir}/body" >/dev/null || die "health endpoint did not return an ok status"

rpc_call() {
  local payload="$1"
  curl "${curl_common[@]}" --fail-with-body -X POST "${rpc_url}/" \
    -H 'Content-Type: application/json' --data-binary "${payload}"
}

chain_response="$(rpc_call '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}')"
jq -e '.jsonrpc == "2.0" and .id == 1 and .result == "0x89"' <<<"${chain_response}" >/dev/null \
  || die "eth_chainId did not identify Polygon (0x89)"

block_response="$(rpc_call '{"jsonrpc":"2.0","id":2,"method":"eth_blockNumber","params":[]}')"
block_number="$(jq -er '.result' <<<"${block_response}")"
[[ "${block_number}" =~ ^0x[0-9A-Fa-f]+$ ]] || die "eth_blockNumber did not return a hexadecimal block number"

batch_response="$(rpc_call '[{"jsonrpc":"2.0","id":11,"method":"eth_chainId","params":[]},{"jsonrpc":"2.0","id":12,"method":"eth_blockNumber","params":[]}]')"
jq -e 'length == 2 and any(.[]; .id == 11 and .result == "0x89") and any(.[]; .id == 12 and (.result | test("^0x[0-9A-Fa-f]+$")))' \
  <<<"${batch_response}" >/dev/null || die "batch JSON-RPC forwarding failed"

request_id="smoke-${RANDOM}-${RANDOM}"
curl "${curl_common[@]}" --fail-with-body -D "${tmp_dir}/headers" -o "${tmp_dir}/body" \
  -X POST "${rpc_url}/" -H 'Content-Type: application/json' -H "X-Request-Id: ${request_id}" \
  --data-binary '{"jsonrpc":"2.0","id":3,"method":"eth_chainId","params":[]}'
grep -Eiq "^x-request-id:[[:space:]]*${request_id}[[:space:]]*\r?$" "${tmp_dir}/headers" \
  || die "a valid client request ID was not preserved"

curl "${curl_common[@]}" --fail-with-body -D "${tmp_dir}/headers" -o "${tmp_dir}/body" \
  -X POST "${rpc_url}/" -H 'Content-Type: application/json' \
  --data-binary '{"jsonrpc":"2.0","id":4,"method":"eth_chainId","params":[]}'
generated_id="$(awk 'tolower($1) == "x-request-id:" {gsub("\r", "", $2); print $2}' "${tmp_dir}/headers" | tail -n 1)"
[[ "${generated_id}" =~ ^[0-9a-f]{32}$ ]] || die "the proxy did not generate a valid request ID"

expect_status 200 "${rpc_url}/metrics"
grep -q '^rpc_proxy_requests_total' "${tmp_dir}/body" || die "proxy metrics were not exposed"
expect_status 405 "${rpc_url}/"
expect_status 404 "${rpc_url}/not-a-route"

dd if=/dev/zero of="${tmp_dir}/oversized" bs=1048577 count=1 status=none
expect_status 413 -X POST "${rpc_url}/" -H 'Content-Type: application/json' --data-binary "@${tmp_dir}/oversized"

if [[ "${scheme}" == "https" ]]; then
  redirect_curl=(--connect-timeout 10 --max-time 30 --retry 3 --retry-delay 2 --retry-max-time 45 --retry-connrefused --silent --show-error)
  if [[ -n "${SMOKE_RESOLVE_IP:-}" ]]; then
    redirect_curl+=(--resolve "${host}:80:${SMOKE_RESOLVE_IP}")
  fi
  redirect_location="$(curl "${redirect_curl[@]}" -o /dev/null -w '%{http_code} %{redirect_url}' "http://${host}/healthz")"
  case "${redirect_location}" in
    "301 https://${host}/healthz" | "301 https://${host}:443/healthz") ;;
    *) die "HTTP did not redirect to the canonical HTTPS endpoint: ${redirect_location}" ;;
  esac
  [[ "${host}" != "${alb_dns}" ]] || die "TLS mode should use the configured public hostname"
fi

log "Checking CloudWatch request logs"
log_found=0
for _ in {1..12}; do
  if aws logs filter-log-events --region "${region}" --log-group-name "${log_group}" \
    --filter-pattern "\"${request_id}\"" --limit 1 --query 'length(events)' --output text 2>/dev/null | grep -q '^1$'; then
    log_found=1
    break
  fi
  sleep 5
done
[[ "${log_found}" == "1" ]] || die "request ${request_id} was not found in CloudWatch Logs within 60 seconds"

log "Checking Terraform drift"
set +e
terraform -chdir="${TF_DIR}" plan -detailed-exitcode -input=false -lock-timeout=60s "${TF_VAR_ARGS[@]}" >/dev/null
plan_status=$?
set -e
[[ "${plan_status}" == "0" ]] || die "Terraform reported drift or could not complete the final plan (exit ${plan_status})"

log "All deployment smoke tests passed"
