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

The binary reads and writes `config.json` in its **working directory**, so run it from the repo root. If the file is missing it writes defaults (hard-coded in `main()`).

## Architecture

**Data flow:** cameras (RTSP) → Frigate (`frigate.yml`) → MQTT topic `frigate/events` (Mosquitto, `docker-compose.yml`) → Go listener → `ssh` to the PBX → `.call` file dropped into `/var/spool/asterisk/outgoing/` → Asterisk dials `pbx_extension` and plays `playback_audio`.

**Go service (`main.go`, single file, package main):**
- All mutable state is package-level globals (`objectStates`, `recentTriggers`, `lastCallTime`, `overrideUntil`, `appConfig`) guarded by one `mu` mutex.
- Alert pipeline, in `processFrigateEvent` → `evaluateAlarmAndCall` → `triggerPBXCall`:
  1. Only `label == "person"` and non-`end` events count. Per-object hit counts are kept by Frigate object ID; stale entries (>10s) are pruned.
  2. **ID bridging:** when a new Frigate ID shows up 100ms–2s after another one disappeared, within `proximity_threshold` pixels of the old box center, its hit count carries over (Frigate often reassigns IDs to the same person).
  3. Every `min_updates` hits on one track adds a "trigger"; `required_triggers` triggers within `trigger_window_minutes` meets the threshold.
  4. Then, in order: outside the alert window (supports wrapping past midnight, e.g. 22:30–04:40) → ignored; pause override active → suppressed; inside `cooldown_minutes` → throttled; `silent_mode` → logged only (still sets `lastCallTime`); otherwise the call is placed. Every outcome clears `recentTriggers`.
- The PBX call is made by shelling out to the system `ssh` binary (key auth as `pbx_user`, `StrictHostKeyChecking=no`); the remote user needs passwordless sudo to `chown`/`mv` into the Asterisk spool.
- HTTP (port `listen_port`, default 8091, served via Apache HTTPS on 8090 in production): `/config` (GET renders the form, POST saves every field), `/toggle-silent`, `/pause`, `/resume`, `/logs` (JSON of the last 5000 log lines, captured by `LogBuffer` as the `log` output).
- `config.html` is compiled into the binary with `//go:embed` and rendered with `html/template`. Rebuild after editing it. When adding a config field, update the `AppConfig` struct, the defaults in `main()`, the POST handler in `handleConfig`, and the form in `config.html` together.

**Frigate (`frigate.yml`):** mounted as `/config/config.yml`; recordings go to `./storage` (gitignored). Camera names here must match the `CAMERAS` list in `set_recordings.sh`. Detection masks are normalized polygons per camera.

## Gotchas / known inconsistencies

- Production runs on **alarm.fsbhoa.com** and is already deployed. Do not run `deploy_listener.sh` or `build.sh` unless asked: they modify systemd, crontab, and Apache. This dev machine (`testbed`) has its own Apache site for testbed.fsbhoa.com.
- MQTT host `192.168.42.62` is hard-coded in both `frigate.yml` and `set_recordings.sh`. The Go service uses `mqtt_host` from `config.json`.
- Mosquitto allows anonymous connections (`mosquitto.conf`).
- `frigate.yml` contains camera RTSP credentials. Be careful when displaying or committing changes to it.
