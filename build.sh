#!/bin/bash

APP_NAME="fsbhoa_intrusion"
SERVICE_NAME="${APP_NAME}.service"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}"
CURRENT_DIR=$(pwd)
USER_NAME=$(whoami)

echo "Building ${APP_NAME}..."
go build -o ${APP_NAME}

if [ $? -ne 0 ]; then
  echo "Build failed! Please check your Go code for errors."
  exit 1
fi
echo "Build successful."

if [ ! -f "${SERVICE_FILE}" ]; then
  echo "Creating systemd service file..."
  sudo tee ${SERVICE_FILE} > /dev/null <<EOF
[Unit]
Description=FSBHOA Pool Intrusion PBX Listener
After=network.target

[Service]
Type=simple
User=${USER_NAME}
WorkingDirectory=${CURRENT_DIR}
ExecStart=${CURRENT_DIR}/${APP_NAME}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

  echo "Reloading systemd daemon..."
  sudo systemctl daemon-reload
  echo "Enabling the service..."
  sudo systemctl enable ${SERVICE_NAME}
else
  echo "Service file already exists, skipping creation..."
fi

echo "Restarting the service..."
sudo systemctl restart ${SERVICE_NAME}

echo "Deployment complete! Live logs: sudo journalctl -u ${SERVICE_NAME} -f"
