#!/usr/bin/env bash
# Builds the provider, runs the mock control plane, and drives the real
# terraform CLI through apply -> update -> replace -> destroy.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${PORT:-8089}"
if [[ -n "${WORK:-}" ]]; then
  KEEP_WORK=1
else
  WORK="$(mktemp -d)"
  KEEP_WORK=0
fi
ENDPOINT="http://127.0.0.1:${PORT}"
export LSMRUST_ENDPOINT="$ENDPOINT"
export LSMRUST_API_KEY="smoke-key"

log() { printf '\n==> %s\n' "$*"; }
fail() { printf '\nSMOKE FAILED: %s\n' "$*" >&2; exit 1; }

MOCK_PID=""
cleanup() {
  [[ -n "$MOCK_PID" ]] && kill "$MOCK_PID" 2>/dev/null || true
  [[ "$KEEP_WORK" == 1 ]] || rm -rf "$WORK"
}
trap cleanup EXIT

log "work dir: $WORK"
rm -rf "$WORK/tf" "$WORK/mirror"
mkdir -p "$WORK/bin" "$WORK/tf"
cp "$ROOT/examples/main/main.tf" "$WORK/tf/"

log "building provider"
(cd "$ROOT" && go build -o "$WORK/bin/terraform-provider-lsmrust" .)

# dev_overrides alone makes `terraform init` fail, because zvdy/lsmrust is not
# in the public registry. The filesystem mirror gives init something to resolve;
# the override still takes precedence for plan/apply/destroy.
PLATFORM="$(go env GOOS)_$(go env GOARCH)"
MIRROR_DIR="$WORK/mirror/registry.terraform.io/zvdy/lsmrust/0.0.1/$PLATFORM"
mkdir -p "$MIRROR_DIR"
cp "$WORK/bin/terraform-provider-lsmrust" "$MIRROR_DIR/terraform-provider-lsmrust_v0.0.1"

cat > "$WORK/dev.tfrc" <<TFRC
provider_installation {
  dev_overrides {
    "registry.terraform.io/zvdy/lsmrust" = "$WORK/bin"
  }
  filesystem_mirror {
    path    = "$WORK/mirror"
    include = ["registry.terraform.io/zvdy/lsmrust"]
  }
  direct {
    exclude = ["registry.terraform.io/zvdy/lsmrust"]
  }
}
TFRC
export TF_CLI_CONFIG_FILE="$WORK/dev.tfrc"
export TF_IN_AUTOMATION=1

log "starting mock control plane on :$PORT"
(cd "$ROOT" && go build -o "$WORK/bin/mock-control-plane" ./mock-control-plane)
"$WORK/bin/mock-control-plane" -addr "127.0.0.1:${PORT}" -api-key "$LSMRUST_API_KEY" >"$WORK/mock.log" 2>&1 &
MOCK_PID=$!
mock_up=0
for _ in $(seq 50); do
  kill -0 "$MOCK_PID" 2>/dev/null || fail "mock control plane exited on startup (port $PORT in use?): $(cat "$WORK/mock.log")"
  if curl -sf --max-time 2 -H "Authorization: Bearer $LSMRUST_API_KEY" "$ENDPOINT/v1/databases" >/dev/null; then
    mock_up=1
    break
  fi
  sleep 0.1
done
[[ "$mock_up" == 1 ]] || fail "mock control plane did not respond on $ENDPOINT within 5s"

cd "$WORK/tf"

plan_expect() {
  local desc="$1" pattern="$2"; shift 2
  local out rc=0
  out="$(terraform plan -no-color -detailed-exitcode "$@" 2>&1)" || rc=$?
  echo "$out"
  [[ $rc -eq 2 ]] || fail "$desc: expected changes (exit 2), got exit $rc"
  grep -qE "$pattern" <<<"$out" || fail "$desc: plan did not match /$pattern/"
}

plan_empty() {
  local rc=0
  terraform plan -no-color -detailed-exitcode "$@" || rc=$?
  [[ $rc -eq 0 ]] || fail "expected an empty plan after apply, got exit $rc"
}

log "terraform init"
terraform init -no-color

log "plan + apply (create)"
plan_expect "create" "Plan: 2 to add, 0 to change, 0 to destroy"
terraform apply -no-color -auto-approve
[[ "$(terraform output -raw database_status)" == "ready" ]] || fail "database not ready after apply"
[[ -n "$(terraform output -raw access_key_secret)" ]] || fail "access key secret empty"
plan_empty

log "update backup retention in place"
plan_expect "retention update" "Plan: 0 to add, 1 to change, 0 to destroy" -var retention=30d
terraform apply -no-color -auto-approve -var retention=30d
plan_empty -var retention=30d

log "change tier (must replace)"
plan_expect "tier change" "tier += \"dedicated\" -> \"shared\" # forces replacement" -var retention=30d -var tier=shared
terraform apply -no-color -auto-approve -var retention=30d -var tier=shared
plan_empty -var retention=30d -var tier=shared

log "destroy"
terraform destroy -no-color -auto-approve -var retention=30d -var tier=shared
remaining="$(curl -sf --max-time 2 -H "Authorization: Bearer $LSMRUST_API_KEY" "$ENDPOINT/v1/databases")"
[[ "$(tr -d '[:space:]' <<<"$remaining")" == "[]" ]] || fail "mock still has databases: $remaining"

log "SMOKE OK"
