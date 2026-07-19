#!/bin/bash

# Assumptions:
# 1) Go is installed.
# 2) Docker has been installed.
# 3) We cloned this repository from github
#      git clone https//github.com/dkeeney/fsbhoa_intrusion.git
# 4) We initialized docker
#      docker compose up -d
# 5) You have created a domain for this machine using LetsEncrypt.
#

# Make sure we're in the right directory
APP_DIR=$(pwd)
SERVICE_FILE="/etc/systemd/system/fsbhoa_intrusion.service"

echo "========================================="
echo " FSBHOA pool intrusion Deployment Script"
echo "========================================="

# 1. Compile the Go application and setup Systemd service
echo "[1/5] Compiling Go application..."
if ! command -v go &> /dev/null; then
    echo "Go is not installed! Please install Go first: sudo apt install golang-go"
    exit 1
fi
chmod +x build.sh
./build.sh
echo "✓ Go Application compiled and service configured."


# 2. Handle SSH Keys for PBX Access
echo "[2/5] Checking SSH keys for PBX authentication..."
if [ ! -f ~/.ssh/id_rsa ]; then
    echo "No SSH key found. Generating one now..."
    ssh-keygen -t rsa -b 4096 -N "" -f ~/.ssh/id_rsa
    echo "✓ SSH key generated."
else
    echo "✓ SSH key already exists."
fi

# 3. Setup Cron Jobs for Recording Schedule
echo "[3/5] Setting up automated recording schedule in crontab..."
if [ -f "$APP_DIR/toggle_recordings.sh" ]; then
    chmod +x "$APP_DIR/toggle_recordings.sh"
    # Backup current crontab, ignoring errors if it doesn't exist
    crontab -l > /tmp/current_cron 2>/dev/null || true
    # Remove any existing entries for our script to avoid duplicates
    grep -v "toggle_recordings.sh" /tmp/current_cron > /tmp/new_cron
    # Append our scheduled tasks
    echo "0 22 * * * $APP_DIR/toggle_recordings.sh ON" >> /tmp/new_cron
    echo "0 5 * * * $APP_DIR/toggle_recordings.sh OFF" >> /tmp/new_cron
    # Install the updated crontab
    crontab /tmp/new_cron
    rm /tmp/current_cron /tmp/new_cron
    echo "✓ Cron jobs installed (ON at 22:00, OFF at 05:00)."
else
    echo "⚠ toggle_recordings.sh not found in $APP_DIR. Skipping cron setup."
fi

# 4. Configure Apache Reverse Proxy
echo "[4/5] Preparing Apache Reverse Proxy for HTTPS..."
if ! command -v apache2 &> /dev/null; then
echo "Installing Apache2..."
sudo apt-get update && sudo apt-get install -y apache2
fi
sudo a2enmod proxy proxy_http proxy_wstunnel ssl

cat << 'EOF' > /tmp/fsbhoa-apps.conf
#NOTE: Replace 'YOUR_DOMAIN_HERE' with your actual domain (e.g. pool.fsbhoa.com)
#      and ensure the path to your Let's Encrypt certificates is correct before enabling.
Listen 8090
Listen 5001

<VirtualHost *:8090>
    ServerName YOUR_DOMAIN_HERE

    SSLEngine on
    SSLCertificateFile /etc/letsencrypt/live/YOUR_DOMAIN_HERE/fullchain.pem
    SSLCertificateKeyFile /etc/letsencrypt/live/YOUR_DOMAIN_HERE/privkey.pem

    ProxyPass / http://127.0.0.1:8091/
    ProxyPassReverse / http://127.0.0.1:8091/
</VirtualHost>


<VirtualHost *:5001>
    ServerName YOUR_DOMAIN_HERE

    SSLEngine on
    SSLCertificateFile /etc/letsencrypt/live/YOUR_DOMAIN_HERE/fullchain.pem
    SSLCertificateKeyFile /etc/letsencrypt/live/YOUR_DOMAIN_HERE/privkey.pem

    # WebSockets support for live video
    ProxyPreserveHost On
    ProxyPass /ws/ ws://127.0.0.1:5000/ws/
    ProxyPassReverse /ws/ ws://127.0.0.1:5000/ws/

    ProxyPass / http://127.0.0.1:5000/
    ProxyPassReverse / http://127.0.0.1:5000/
</VirtualHost>
EOF

# 5. Final Instructions
echo "========================================="
echo "Deployment Complete!"
echo "The listener is now running in the background."
echo "You can view logs at any time using: sudo journalctl -u fsbhoa_intrusion.service -f"
echo ""
echo "[5/5] CRITICAL NEXT STEP:"
echo "You must copy your SSH key to the PBX server so the listener can log in without a password."
echo "Run this command manually (it will ask for the PBX password one last time):"
echo ""
echo "  ssh-copy-id -i ~/.ssh/id_rsa.pub fsbhoa@<YOUR_PBX_IP>"
echo ""
echo "Edit your Apache proxy config file with your domain and enable it:"
echo "   sudo vi /etc/apache2/sites-available/<YOUR_DOMAIN>.conf"
echo "      append the contents of /tmp/fsbhoa-apps.conf"
echo "   sudo systemctl restart apache2"
echo "   sudo systemctl restart fsbhoa_intrusion.service"
echo "Then, open https://<YOUR_DOMAIN>:8090/config to configure your settings."
echo "========================================="

