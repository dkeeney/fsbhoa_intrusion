#!/bin/bash

# Deployment script for the FSBHOA pool alarm system (alarm.fsbhoa.com).
# Safe to re-run: every step checks for existing state before changing it.
#
# Assumptions:
# 1) Go and Docker (with the compose plugin) are installed.
# 2) This repository is cloned on the alarm server and this script is run from its root:
#      git clone https://github.com/dkeeney/fsbhoa_intrusion.git
# 3) A Let's Encrypt certificate exists for alarm.fsbhoa.com
#      (/etc/letsencrypt/live/alarm.fsbhoa.com/).

set -e

DOMAIN="alarm.fsbhoa.com"
PBX_USER="fsbhoa"
PBX_HOST="192.168.40.100"

APP_DIR=$(cd "$(dirname "$0")" && pwd)
cd "$APP_DIR"
CERT_DIR="/etc/letsencrypt/live/${DOMAIN}"
APACHE_SITE="/etc/apache2/sites-available/${DOMAIN}-pool.conf"

echo "========================================="
echo " FSBHOA Pool Alarm Deployment (${DOMAIN})"
echo "========================================="

# 1. Start Frigate and Mosquitto
echo "[1/6] Starting Frigate and Mosquitto containers..."
if ! command -v docker &> /dev/null; then
    echo "Docker is not installed! Please install Docker first."
    exit 1
fi
docker compose up -d
echo "✓ Containers running."

# 2. Compile the Go application and setup Systemd service
echo "[2/6] Compiling Go application..."
if ! command -v go &> /dev/null; then
    echo "Go is not installed! Please install Go first: sudo apt install golang-go"
    exit 1
fi
chmod +x build.sh
./build.sh
echo "✓ Go Application compiled and service configured."

# 3. Handle SSH Keys for PBX Access
echo "[3/6] Checking SSH keys for PBX authentication..."
if [ ! -f ~/.ssh/id_rsa ]; then
    echo "No SSH key found. Generating one now..."
    ssh-keygen -t rsa -b 4096 -N "" -f ~/.ssh/id_rsa
    echo "✓ SSH key generated."
else
    echo "✓ SSH key already exists."
fi

# 4. Setup Cron Jobs for Recording Schedule
echo "[4/6] Setting up automated recording schedule in crontab..."
if ! command -v mosquitto_pub &> /dev/null; then
    echo "Installing mosquitto-clients (needed by set_recordings.sh)..."
    sudo apt-get update && sudo apt-get install -y mosquitto-clients
fi
chmod +x "$APP_DIR/set_recordings.sh"
NEW_CRON=$(mktemp)
# Keep existing entries except our own, to avoid duplicates
crontab -l 2>/dev/null | grep -v "set_recordings.sh" > "$NEW_CRON" || true
echo "0 22 * * * $APP_DIR/set_recordings.sh ON" >> "$NEW_CRON"
echo "0 5 * * * $APP_DIR/set_recordings.sh OFF" >> "$NEW_CRON"
crontab "$NEW_CRON"
rm "$NEW_CRON"
echo "✓ Cron jobs installed (ON at 22:00, OFF at 05:00)."

# 5. Configure Apache Reverse Proxy
#    https://alarm.fsbhoa.com:8090 -> Go config portal (127.0.0.1:8091)
#    https://alarm.fsbhoa.com:5001 -> Frigate UI with websockets (127.0.0.1:5000)
echo "[5/6] Configuring Apache reverse proxy for HTTPS..."
if ! command -v apache2 &> /dev/null; then
    echo "Installing Apache2..."
    sudo apt-get update && sudo apt-get install -y apache2
fi
if [ ! -f "${CERT_DIR}/fullchain.pem" ]; then
    echo "Certificate not found in ${CERT_DIR}. Obtain it first, e.g.:"
    echo "   sudo certbot --apache -d ${DOMAIN}"
    exit 1
fi
sudo a2enmod proxy proxy_http proxy_wstunnel ssl rewrite

for PORT in 8090 5001; do
    if ! grep -qE "^\s*Listen\s+${PORT}\b" /etc/apache2/ports.conf; then
        echo "Listen ${PORT}" | sudo tee -a /etc/apache2/ports.conf > /dev/null
    fi
done

if [ ! -f "${APACHE_SITE}" ]; then
    sudo tee "${APACHE_SITE}" > /dev/null <<EOF
<VirtualHost *:8090>
    ServerName ${DOMAIN}

    SSLEngine on
    SSLCertificateFile ${CERT_DIR}/fullchain.pem
    SSLCertificateKeyFile ${CERT_DIR}/privkey.pem

    ProxyPass / http://127.0.0.1:8091/
    ProxyPassReverse / http://127.0.0.1:8091/
</VirtualHost>

<VirtualHost *:5001>
    ServerName ${DOMAIN}

    SSLEngine on
    SSLCertificateFile ${CERT_DIR}/fullchain.pem
    SSLCertificateKeyFile ${CERT_DIR}/privkey.pem

    # WebSockets support for live video
    ProxyPreserveHost On
    RewriteEngine On
    RewriteCond %{HTTP:Upgrade} =websocket [NC]
    RewriteRule /(.*)  ws://127.0.0.1:5000/\$1 [P,L]

    ProxyPass / http://127.0.0.1:5000/
    ProxyPassReverse / http://127.0.0.1:5000/
</VirtualHost>
EOF
    sudo a2ensite "$(basename "${APACHE_SITE}")"
    echo "✓ Created and enabled ${APACHE_SITE}."
else
    echo "✓ ${APACHE_SITE} already exists, leaving it unchanged."
fi
sudo apache2ctl configtest
sudo systemctl reload apache2
echo "✓ Apache reloaded."

# 6. Final Instructions
echo "========================================="
echo "Deployment Complete!"
echo "Listener logs: sudo journalctl -u fsbhoa_intrusion.service -f"
echo ""
echo "[6/6] ONE-TIME MANUAL STEP (skip if already done):"
echo "Copy the SSH key to the PBX so the listener can place calls without a password."
echo "It will ask for the PBX password one last time:"
echo ""
echo "  ssh-copy-id -i ~/.ssh/id_rsa.pub ${PBX_USER}@${PBX_HOST}"
echo ""
echo "Config portal: https://${DOMAIN}:8090/config"
echo "Frigate UI:    https://${DOMAIN}:5001"
echo "========================================="
