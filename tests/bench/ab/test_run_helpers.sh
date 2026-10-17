#!/usr/bin/env bash
set -Eeuo pipefail

here=$(cd "$(dirname "$0")" && pwd)
. "$here/run_helpers.sh"
delivery_projects=()
delivery_compose_files=()
delivery_cleanup_failed=0

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cat > "$tmp/docker" <<'DOCKER'
#!/usr/bin/env bash
set -eu
printf 'docker:%s\n' "$*" >> "$BENCH_HELPER_TEST_LOG"
if [[ "$*" == "compose ls --all --quiet" ]]; then
  printf '%s\n' "${BENCH_HELPER_TEST_PROJECTS:-}"
fi
exit "${BENCH_HELPER_TEST_DOCKER_EXIT:-0}"
DOCKER
chmod +x "$tmp/docker"
export PATH="$tmp:$PATH"
export BENCH_HELPER_TEST_LOG="$tmp/commands.log"

if bench_validate_project_name mk; then
  echo "unsafe project name was accepted" >&2
  exit 1
fi
bench_validate_project_name mk-bench-ab-safe_1

export BENCH_HELPER_TEST_PROJECTS=mk-bench-ab-existing
if bench_assert_project_available mk-bench-ab-existing; then
  echo "existing project name was accepted" >&2
  exit 1
fi
if [[ $(cat "$BENCH_HELPER_TEST_LOG") == *"down"* ]]; then
  echo "collision check issued a destructive down" >&2
  exit 1
fi

fail_driver() {
  printf 'driver:failed\n' >> "$BENCH_HELPER_TEST_LOG"
  return 17
}
export BENCH_HELPER_TEST_PROJECTS=
delivery_projects+=(mk-bench-ab-safe_1-delivery-r1-base)
delivery_compose_files+=(/tmp/isolated-compose.yml)
status=0
bench_run_delivery_with_cleanup mk-bench-ab-safe_1-delivery-r1-base \
  /tmp/isolated-compose.yml fail_driver || status=$?
[[ "$status" -eq 17 ]]
commands=$(cat "$BENCH_HELPER_TEST_LOG")
[[ "$commands" == *"driver:failed"*"docker:compose -p mk-bench-ab-safe_1-delivery-r1-base -f /tmp/isolated-compose.yml --profile bench --profile outbound --profile delivery down -v --remove-orphans --rmi local"* ]]
[[ "${#delivery_projects[@]}" -eq 0 ]]

succeed_driver() { return 0; }
delivery_projects+=(mk-bench-ab-safe_1-delivery-r2-head)
delivery_compose_files+=(/tmp/isolated-compose.yml)
export BENCH_HELPER_TEST_DOCKER_EXIT=1
status=0
bench_run_delivery_with_cleanup mk-bench-ab-safe_1-delivery-r2-head \
  /tmp/isolated-compose.yml succeed_driver || status=$?
[[ "$status" -eq 1 ]]
[[ "$delivery_cleanup_failed" -eq 1 ]]
[[ "${#delivery_projects[@]}" -eq 1 ]]

: > "$BENCH_HELPER_TEST_LOG"
bench_restart_delivery_stack docker compose -p mk-bench-ab-order -f /tmp/order-compose.yml
expected_restart_order=$(cat <<'EXPECTED'
docker:compose -p mk-bench-ab-order -f /tmp/order-compose.yml restart --no-deps app-mkq
docker:compose -p mk-bench-ab-order -f /tmp/order-compose.yml --no-build up -d --wait app-mkq
docker:compose -p mk-bench-ab-order -f /tmp/order-compose.yml restart --no-deps nginx-mkq
docker:compose -p mk-bench-ab-order -f /tmp/order-compose.yml --no-build up -d --wait nginx-mkq
EXPECTED
)
[[ "$(cat "$BENCH_HELPER_TEST_LOG")" == "$expected_restart_order" ]]
echo "run helper checks passed"
