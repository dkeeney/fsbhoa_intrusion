# FSBHOA Pool Intrusion Detection System

Production deployment: **alarm.fsbhoa.com**

## Architecture Overview

This repository contains the configuration and automation logic for the FSBHOA after-hours pool security system. Real-time AI object detection looks for people in the pool area during a configurable night window (for example 22:30–04:40) and automatically alerts management with an outbound phone call.

The stack has four components:

- **Frigate NVR** (Docker): ingests the RTSP streams from the pool cameras, runs person detection, and records video. Configured in `frigate.yml`.
- **Mosquitto MQTT broker** (Docker): Frigate publishes detection events to the `frigate/events` topic.
- **Pool alarm listener** (Go, `main.go`): subscribes to `frigate/events` and filters out false positives. It requires a sustained person track, repeated triggers within a time window, the alert time window, and a cooldown. When all of these are met, it uses SSH to drop an Asterisk `.call` file onto the Incredible PBX server, which phones the alert extension. It also serves a web configuration portal.
- **Apache reverse proxy**: provides HTTPS using the Let's Encrypt certificate for alarm.fsbhoa.com.

## Web Interfaces

| URL | Purpose |
|-----|---------|
| `https://alarm.fsbhoa.com:8090/config` | Alarm settings, silent-mode toggle, pause/resume, and live logs (proxied to the Go listener on port 8091) |
| `https://alarm.fsbhoa.com:5001` | Frigate UI, live video, and recordings (proxied to Frigate on port 5000) |

Settings saved in the portal are written to `config.json` in the repository directory.

## Hardware Stack

- **Production server:** Beelink EQi13 Mini PC (Intel i5-13500H, 32GB RAM). It uses dual LAN to isolate camera traffic from the main HOA network.
- **Telephony:** Incredible PBX (Asterisk) routed through Skyetel SIP trunks.
- **Cameras:** IP cameras (Speco O2VLB5, Amcrest IP5M-B1186E).

## Repository Structure

```
.
├── main.go               Go listener: MQTT consumer, alert logic, PBX dialer, web portal
├── config.html           Web portal template (embedded into the binary)
├── config.json           Runtime settings (edited through the web portal)
├── docker-compose.yml    Frigate + Mosquitto containers
├── frigate.yml           Frigate configuration: cameras, detection, masks
├── mosquitto.conf        MQTT broker configuration
├── build.sh              Builds the listener and installs/restarts its systemd service
├── deploy_listener.sh    Full server deployment for alarm.fsbhoa.com
└── set_recordings.sh     Turns Frigate recording ON/OFF for all cameras (run by cron)
```

## Deployment

Run these steps on the alarm server from the repository root. You need Go, Docker, and a Let's Encrypt certificate for alarm.fsbhoa.com.

```bash
git clone https://github.com/dkeeney/fsbhoa_intrusion.git
cd fsbhoa_intrusion
./deploy_listener.sh
```

`deploy_listener.sh` is safe to re-run. It:

1. Starts the Frigate and Mosquitto containers (`docker compose up -d`).
2. Builds the Go listener and installs it as the `fsbhoa_intrusion` systemd service (`build.sh`).
3. Creates an SSH key for PBX access if one doesn't already exist.
4. Installs cron jobs that turn recording ON at 22:00 and OFF at 05:00 (`set_recordings.sh`).
5. Sets up the Apache HTTPS proxy on ports 8090 and 5001 (`/etc/apache2/sites-available/alarm.fsbhoa.com-pool.conf`).

One manual step is needed the first time: copy the SSH key to the PBX.

```bash
ssh-copy-id -i ~/.ssh/id_rsa.pub fsbhoa@192.168.40.100
```

### Updating code on the running system

```bash
git pull
./build.sh                        # rebuild and restart the listener
docker compose restart frigate    # after editing frigate.yml
```

Listener logs: `sudo journalctl -u fsbhoa_intrusion.service -f`
