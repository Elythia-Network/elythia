#!/usr/bin/env bash

# Keep all Compose project names under the benchmark namespace. run.sh also
# checks that a selected name is not already present before it can run `down`.
bench_validate_project_name() {
  local name=$1
  [[ "$name" =~ ^mk-bench-ab-[a-z0-9][a-z0-9_-]{0,19}$ ]]
}

bench_assert_project_available() {
  local project=$1 existing name
  if ! existing=$(docker compose ls --all --quiet); then
    echo "run.sh: could not inspect existing Compose projects" >&2
    return 1
  fi
  while IFS= read -r name; do
    if [[ "$name" == "$project" ]]; then
      echo "run.sh: Compose project '$project' already exists; choose a fresh benchmark name" >&2
      return 1
    fi
  done <<< "$existing"
}

bench_cleanup_delivery_project() {
  local project=$1 compose_file=$2
  docker compose -p "$project" -f "$compose_file" \
    --profile bench --profile outbound --profile delivery \
    down -v --remove-orphans --rmi local >/dev/null 2>&1
}

# nginx resolves app-mkq's container address when nginx starts. Restart the
# app first and wait for it to become healthy before restarting nginx.
bench_restart_delivery_stack() {
  local -a compose=("$@")
  "${compose[@]}" restart --no-deps app-mkq || return $?
  "${compose[@]}" --no-build up -d --wait app-mkq || return $?
  "${compose[@]}" restart --no-deps nginx-mkq || return $?
  "${compose[@]}" --no-build up -d --wait nginx-mkq
}

bench_forget_delivery_project() {
  local project=$1 i
  for i in "${!delivery_projects[@]}"; do
    if [[ "${delivery_projects[$i]}" == "$project" ]]; then
      unset "delivery_projects[$i]" "delivery_compose_files[$i]"
    fi
  done
}

# Always tear down a delivery fixture before returning to the HTTP rounds,
# including when its build, setup, seed, or driver fails.
bench_run_delivery_with_cleanup() {
  local project=$1 compose_file=$2 status=0 cleanup_status=0
  shift 2
  "$@" || status=$?
  bench_cleanup_delivery_project "$project" "$compose_file" || cleanup_status=$?
  if [[ "$cleanup_status" -eq 0 ]]; then
    bench_forget_delivery_project "$project"
  else
    delivery_reason="delivery fixture cleanup failed"
    delivery_cleanup_failed=1
    status=1
  fi
  return "$status"
}
