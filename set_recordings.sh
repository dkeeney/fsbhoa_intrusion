#!/bin/bash

# Convert the first argument to uppercase to allow 'on' or 'ON'
STATE=$(echo "$1" | tr '[:lower:]' '[:upper:]')
MQTT_HOST="192.168.42.62"

if [ "$STATE" != "ON" ] && [ "$STATE" != "OFF" ]; then
    echo "Usage: $0 [ON|OFF]"
    echo "Example: $0 ON"
    exit 1
fi

CAMERAS=("pool_from_cabana" "Pool_SW" "jacuzzi" "Pool_NE")

for CAM in "${CAMERAS[@]}"; do
    mosquitto_pub -h "$MQTT_HOST" -t "frigate/${CAM}/record/set" -m "$STATE"
done

echo "Successfully sent $STATE command to all cameras."
```

