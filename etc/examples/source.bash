REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

run() {
  echo $@ >&2
  $@
}

run_make() {
  run make -C "${REPO_DIR}" $@
}

do_pps() {
  if [ -z ${PPS_CLIENT_EXAMPLE} ]; then
    run_make launch-ppsd
    run docker run -e PPS_CLIENT_EXAMPLE=1 --link pachyderm_ppsd_1 pachyderm_compile bash "etc/examples/$(basename "${1}")"
    run_make docker-clean-launch
  else
    export PPS_ADDRESS="$(echo "${PACHYDERM_PPSD_1_PORT}" | sed "s/tcp:\/\///")"
    run_make install
    ${2}
  fi
}
