#!/usr/bin/env bash
# Build a local container image with the first usable supported engine.

set -euo pipefail

# shellcheck source=scripts/aws-common.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/aws-common.sh"

image_name="${1:-polygon-rpc-proxy}"
image_tag="${2:-dev}"
image_ref="${image_name}:${image_tag}"
engine="$(select_container_engine)"

log "Container engine: ${engine}"
log "Building ${image_ref}"
cd "${REPO_ROOT}"
"${engine}" build -t "${image_ref}" .
