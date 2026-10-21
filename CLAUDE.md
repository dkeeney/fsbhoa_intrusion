# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

After-hours intrusion detection for the FSBHOA pool. Frigate NVR runs AI person detection on the pool cameras and publishes events over MQTT. A Go service (`main.go`) consumes those events, decides whether a real intrusion is happening, and places an alert phone call through an Asterisk (Incredible PBX) server.

## Commands

```bash
go build -o fsbhoa_intrusion      # build only
go vet ./...                      # static checks (there are no tests in the repo)
./build.sh                        # build + create/enable/restart the systemd service (uses sudo)
./deploy_listener.sh              # full deploy for alarm.fsbhoa.com (containers, service, SSH key, cron, Apache vhosts); idempotent
sudo journalctl -u fsbhoa_intrusion.service -f   # live service logs

docker compose up -d              # start Mosquitto (1883) + Frigate (UI 5000/8971, RTSP 8554, WebRTC 8555)
docker compose restart frigate    # pick up frigate.yml edits
./set_recordings.sh ON|OFF        # toggle Frigate recording on all cameras via mosquitto_pub
```

The binary reads and writes `config.json` in its **working directory**, so run it from the repo root. If the file is missing it writes defaults (hard-coded in `main()`). `config.json` is gitignored (it holds the WU API key); `config.example.json` mirrors the `main()` defaults, so keep the two in sync when adding a field.

## Architecture

**Data flow:** cameras (RTSP) → Frigate (`frigate.yml`) → MQTT topic `frigate/events` (Mosquitto, `docker-compose.yml`) → Go listener → `ssh` to the PBX → `.call` file dropped into `/var/spool/asterisk/outgoing/` → Asterisk dials `pbx_extension` and plays `playback_audio`.

**Go service (`main.go`, single file, package main):**
- All mutable state is package-level globals (`objectStates`, `recentTriggers`, `lastCallTime`, `overrideUntil`, `appConfig`) guarded by one `mu` mutex; weather state (`isStormy`, `weatherReason`, `weatherSource`) has its own `weatherMutex`.
- Background goroutines: `startMQTTListener`, `monitorWeather` (polls every 10 min), and an `init()` loop that reads per-camera person `min_score` from Frigate's API (`127.0.0.1:5000/api/config`).
- Alert pipeline, in `processFrigateEvent` → `evaluateAlarmAndCall` → `triggerPBXCall`:
  1. Only `label == "person"` events count; an `end` event logs a `[TRACK END]` summary and drops the track. Tracks idle >60s are pruned.
  2. **ID bridging:** when a new Frigate ID shows up 100ms–3s after another one went quiet, within `proximity_threshold` pixels of its box center, it inherits that track (Frigate often reassigns IDs to the same person).
  3. Each update adds its confidence (floored at the camera's `min_score`) to the track's `TotalConf`; every `min_updates` worth of accumulated confidence awards one trigger. `required_triggers` triggers within `trigger_window_minutes` meets the threshold.
  4. Then, in order: outside the alert window (wraps past midnight, e.g. 22:30–04:40) → ignored; pause/override → suppressed; **weather lockout** (`isStormy`) → suppressed; inside `cooldown_minutes` → throttled; `silent_mode` → logged only (still sets `lastCallTime`); otherwise the call is placed. Every outcome clears `recentTriggers`.
- **Weather lockout:** gusts above `wind_threshold` mph or active rain set `isStormy`. Source is the Weather Underground PWS `wu_station_id` (a block from the pool) when `wu_api_key` is set, otherwise or on failure the NWS station `nws_station_id` (default KBFL, several miles away, no key). If both fail the previous state is kept (retrying every minute until the first good reading); the page shows "Weather unavailable" until the first good reading. Do not hard-code a WU key scraped from wunderground.com: the old shared one was throttled (HTTP 401), which silently disabled the lockout.
- The PBX call is made by shelling out to the system `ssh` binary (key auth as `pbx_user`, `StrictHostKeyChecking=no`); the remote user needs passwordless sudo to `chown`/`mv` into the Asterisk spool. The call plays `playback_audio` four times.
- HTTP (port `listen_port`, default 8091, served via Apache HTTPS on 8090 in production): `/config` (GET renders the form, POST saves every field), `/toggle-silent`, `/pause` (for `override_minutes`), `/override` (pause indefinitely), `/resume`, `/test-alarm` (POST, places a real PBX call), `/weather-status`, `/logs` (JSON of the last 5000 log lines, captured by `LogBuffer` as the `log` output, with 12-hour timestamps). `/logs` filters by `log_filter_mode` (0 all, 1 triggers and alarms, 2 alarms only) using hard-coded tag lists such as `[TRACKING]` and `[WEATHER]`; a new log tag that should stay visible must be added to those lists.
- `config.html` is compiled into the binary with `//go:embed` and rendered with `html/template`. Rebuild after editing it. When adding a config field, update the `AppConfig` struct, the defaults in `main()`, the POST handler in `handleConfig`, and the form in `config.html` together.

**Frigate (`frigate.yml`):** uses the OpenVINO detector on the Intel GPU (`/dev/dri`); talks to the `mosquitto` container by name on the `frigate_net` Docker network. Mounted as `/config/config.yml`; recordings go to `./storage` (gitignored). Camera names here must match the `CAMERAS` list in `set_recordings.sh`. Detection masks are normalized polygons per camera. The Speco O2VLB5 cameras (.19 `jacuzzi`, .51 `pool_from_cabana`, RTSP port 9008) offer only `profile1` 1080p, `profile2` 704×480 and `profile3` 352×240; there is no `profile0`. Ask a camera over ONVIF (`GetProfiles`/`GetStreamUri` on `/onvif/Media`) before guessing stream paths.

## Production (alarm.fsbhoa.com)

- SSH as `fsbhoa@alarm.fsbhoa.com`; repo at `~/fsbhoa_intrusion`; passwordless sudo. Allowed in `.claude/settings.local.json`; SSH commands mentioning frigate or docker still prompt.
- Deploy: run `git status` there first (production has had uncommitted edits; merge them into the repo rather than discard), then `git pull --ff-only` and `./build.sh`. That restarts only the listener.
- Frigate settings are tuned on production: change `frigate.yml` only with the user's OK, and only restart Frigate in daytime, outside the 22:30–04:40 alert window. Use `docker compose restart frigate` after `frigate.yml` changes, and `docker compose up -d frigate` after `docker-compose.yml` or `.env` changes.
- Network: the server is on VLAN 42 (192.168.42.63, static, port enp3s0, only `/etc/netplan/01-static-ip.yaml`; cloud-init networking disabled). Cameras are on VLANs 78 and 107, reached through VLAN 42's routing. The second NIC (enp2s0) is unused. Testbed is 192.168.42.62. Apply remote network changes with an automatic rollback timer (`systemd-run --on-active=…`) armed first.
- No monitor is attached. A warm reboot hung once (2026-10-21) and needed a power cycle at the Lodge, so reboot only when someone can get to it.

## Gotchas / known inconsistencies

- Production runs on **alarm.fsbhoa.com** and is already deployed. Do not run `deploy_listener.sh` or `build.sh` unless asked: they modify systemd, crontab, and Apache. This dev machine (`testbed`) has its own Apache site for testbed.fsbhoa.com.
- `set_recordings.sh` hard-codes MQTT host `192.168.42.62`, which is testbed, not production; Frigate reaches the broker as `mosquitto` and the Go service uses `mqtt_host` from `config.json`.
- Before changing code, `git fetch`: production commits directly to GitHub `main`, so local `main` may be behind.
- `main.go` is not `gofmt`-clean; avoid reformatting unrelated lines in a fix.
- Mosquitto allows anonymous connections (`mosquitto.conf`).
- Camera RTSP passwords live only in the gitignored `.env` (loaded by `docker-compose.yml` via `env_file`); `frigate.yml` uses `{FRIGATE_CAM_PASSWORD_N}` placeholders. Never put a literal password back into `frigate.yml`. Old passwords remain in git history (the repo is public).
