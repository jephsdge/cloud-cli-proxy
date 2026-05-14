#!/usr/bin/env bash
set -euo pipefail

export DISPLAY="${DISPLAY:-:99}"
export HOME="${HOME:-/workspace}"

browser_cmd=""
for candidate in chromium chromium-browser google-chrome; do
  if command -v "${candidate}" >/dev/null 2>&1; then
    browser_cmd="${candidate}"
    break
  fi
done

if [[ -z "${browser_cmd}" ]]; then
  exec xterm -fa Monospace -fs 12 -geometry 120x30+60+60 -title "cloud-cli-proxy desktop" \
    -e bash -lc "echo Chromium is not installed.; exec bash"
fi

browser_language="${CLOUDPROXY_BROWSER_LANGUAGE:-en-US}"
browser_window_size="${CLOUDPROXY_BROWSER_WINDOW_SIZE:-1920x1080}"
if ! [[ "${browser_window_size}" =~ ^[0-9]{3,4}x[0-9]{3,4}$ ]]; then
  browser_window_size="1920x1080"
fi
browser_window_size="${browser_window_size/x/,}"

if [[ $# -gt 0 ]]; then
  exec "${browser_cmd}" "$@"
fi

exec "${browser_cmd}" \
  --no-sandbox \
  --disable-dev-shm-usage \
  --user-data-dir=/workspace/.chrome-data \
  --start-maximized \
  --no-first-run \
  --disable-gpu \
  --disable-features=WebRtcHideLocalIpsWithMdns \
  --enforce-webrtc-ip-permission-check \
  --force-webrtc-ip-handling-policy=disable_non_proxied_udp \
  --lang="${browser_language}" \
  --window-position=0,0 \
  --window-size="${browser_window_size}" \
  "https://www.google.com"
