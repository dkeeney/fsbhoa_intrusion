#FSBHOA Pool Intrusion Detection System

##Architecture Overview

This repository contains the configuration and automation logic for the FSBHOA after-hours pool security system. The system utilizes real-time AI object detection to identify humans in the pool area between 10:00 PM and 5:00 AM, and automatically alerts management via an outbound phone call.

The stack consists of three primary components:

Frigate NVR: An open-source NVR running in Docker that ingests the RTSP streams from the Speco and Amcrest pool cameras.

AI Inference (OpenVINO): Utilizes the integrated GPU on the Beelink i5-13500H to run human-detection AI models against the video streams without overloading the CPU.

PBX Listener (Go): A custom Go microservice that listens for Frigate webhook events, verifies the time-of-day constraints, and pushes an Asterisk .call file to the Incredible PBX server over SSH to initiate an automated alert call.

##Hardware Stack

Production Environment: Beelink EQi13 Mini PC (Intel i5-13500H, 32GB RAM).

Note: Utilizes dual-LAN to isolate camera traffic from the main HOA network.

Telephony: Incredible PBX (Asterisk) routed via Skyetel SIP trunks.

Cameras: 4x IP Cameras (Speco O2VLB5, Amcrest IP5M-B1186E).

##Repository Structure

.
├── docker-compose.yml       (Docker definition for Frigate NVR)
├── frigate.yml              (Frigate configuration - Cameras, AI, Masks)
├── cmd/
│   └── pbx-listener/
│       └── main.go          (The Go webhook listener and SSH dialer)
└── README.md
