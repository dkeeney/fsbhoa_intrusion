#!/bin/bash

# Convert the first argument to uppercase to allow 'on' or 'ON'
STATE=$(echo "$1" | tr '[:lower:]' '[:upper:]')
# Runs on the alarm server, where Mosquitto listens on local port 1883
MQTT_HOST="${MQTT_HOST:-127.0.0.1}"

if [ "$STATE" != "ON" ] && [ "$STATE" != "OFF" ]; then
    echo "Usage: $0 [ON|OFF]"
    echo "Example: $0 ON"
    exit 1
fi

CAMERAS=("pool_from_cabana" "Pool_SW" "jacuzzi" "Pool_NE" "Pool_farside")

FAILED=0
for CAM in "${CAMERAS[@]}"; do
    # Frigate subscribes to <prefix>/<camera>/recordings/set
    if ! mosquitto_pub -h "$MQTT_HOST" -t "frigate/${CAM}/recordings/set" -m "$STATE"; then
        echo "Failed to send $STATE to $CAM" >&2
        FAILED=1
    fi
done

if [ "$FAILED" -ne 0 ]; then
    exit 1
fi
echo "Successfully sent $STATE command to all cameras."
