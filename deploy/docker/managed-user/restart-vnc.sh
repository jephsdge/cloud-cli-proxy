#!/usr/bin/env bash
set -euo pipefail

if [ "${EUID:-$(id -u)}" -ne 0 ]; then
  exec sudo -n /usr/local/bin/restart-vnc "$@"
fi

RUN_USER="${CONTAINER_USER:-workspace}"
if ! id "${RUN_USER}" >/dev/null 2>&1; then
  RUN_USER="workspace"
fi
RUN_UID="$(id -u "${RUN_USER}")"
RUN_GID="$(id -g "${RUN_USER}")"
RUN_HOME="${CONTAINER_HOME:-$(getent passwd "${RUN_USER}" | cut -d: -f6)}"
RUN_HOME="${RUN_HOME:-/workspace}"

LOG_DIR="${RUN_HOME}/.vnc"
XVNC_LOG="${LOG_DIR}/xvnc.log"
FLUXBOX_LOG="${LOG_DIR}/fluxbox.log"
DESKTOP_LOG="${LOG_DIR}/desktop.log"
VNC_RESOLUTION="${CLOUDPROXY_VNC_RESOLUTION:-1920x1080}"
if ! [[ "${VNC_RESOLUTION}" =~ ^[0-9]{3,4}x[0-9]{3,4}$ ]]; then
  VNC_RESOLUTION="1920x1080"
fi

mkdir -p "${LOG_DIR}" /tmp/.X11-unix
chmod 1777 /tmp/.X11-unix
touch "${XVNC_LOG}" "${FLUXBOX_LOG}" "${DESKTOP_LOG}"
chown -R "${RUN_UID}:${RUN_GID}" "${LOG_DIR}"

pkill -f 'Xvnc :99' || true
pkill -u "${RUN_USER}" -x fluxbox || true
pkill -u "${RUN_USER}" -f 'pcmanfm --desktop --profile default' || true

export DISPLAY=:99
su "${RUN_USER}" -c 'Xvnc :99 \
  -geometry '"${VNC_RESOLUTION}"' \
  -depth 24 \
  -websocketPort 6080 \
  -SecurityTypes None \
  -interface 0.0.0.0 \
  -BlacklistThreshold 0 \
  -FreeKeyMappings \
  -disableBasicAuth \
  -publicIP 127.0.0.1 \
  -httpd /usr/share/kasmvnc/www' >>"${XVNC_LOG}" 2>&1 &

ready=0
for _ in $(seq 1 30); do
  if DISPLAY=:99 xdpyinfo >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done

if [ "${ready}" -ne 1 ]; then
  echo "Xvnc did not become ready on DISPLAY :99 within 30 seconds" >>"${XVNC_LOG}"
  tail -n 80 "${XVNC_LOG}" >&2 || true
  exit 1
fi

su "${RUN_USER}" -c 'DISPLAY=:99 xsetroot -solid "#17324d"' >/dev/null 2>&1 || true
su "${RUN_USER}" -c 'DISPLAY=:99 fluxbox' >>"${FLUXBOX_LOG}" 2>&1 &
su "${RUN_USER}" -c "DISPLAY=:99 HOME=${RUN_HOME@Q} pcmanfm --desktop --profile default" >>"${DESKTOP_LOG}" 2>&1 &

echo "VNC restarted (display=:99 websocket=6080 user=${RUN_USER})"
