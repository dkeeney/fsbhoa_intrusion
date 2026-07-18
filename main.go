package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

//go:embed config.html
var configHTML string

// AppConfig holds the adjustable thresholds
type AppConfig struct {
	MinUpdates           int    `json:"min_updates"`
	RequiredTriggers     int    `json:"required_triggers"`
	TriggerWindowMinutes int    `json:"trigger_window_minutes"`
	ProximityThreshold   int    `json:"proximity_threshold"` // Max pixels an object can move between dropped frames
	AlertWindowStart     string `json:"alert_window_start"`
	AlertWindowEnd       string `json:"alert_window_end"`
	CooldownMinutes      int    `json:"cooldown_minutes"`
	OverrideMinutes      int    `json:"override_minutes"`
	ListenPort           int    `json:"listen_port"`
	PBXHost              string `json:"pbx_host"`
	PBXUser              string `json:"pbx_user"`
	PBXExtension         string `json:"pbx_extension"`
	CallerID             string `json:"caller_id"`
	PlaybackAudio        string `json:"playback_audio"`
	MQTTHost             string `json:"mqtt_host"`
}

// FrigateEvent represents the incoming JSON payload from Frigate over MQTT
type FrigateEvent struct {
	Type  string `json:"type"`
	After struct {
		ID       string    `json:"id"`
		Label    string    `json:"label"`
		Camera   string    `json:"camera"`
		TopScore float64   `json:"top_score"`
		Box      []float64 `json:"box"` // [ymin, xmin, ymax, xmax]
	} `json:"after"`
}

// ObjectState tracks the last known physical position of a person
type ObjectState struct {
	HitCount int
	LastSeen time.Time
	LastX    float64
	LastY    float64
}

var (
	objectStates   = make(map[string]*ObjectState)
	recentTriggers []time.Time
	lastCallTime   time.Time
	overrideUntil  time.Time
	mu             sync.Mutex
	appConfig      AppConfig
)

func main() {
	// Load configuration from file
	configFile, err := os.ReadFile("config.json")
	if err != nil {
		log.Println("config.json not found, creating default configuration...")
		appConfig = AppConfig{
			MinUpdates:           3,
			RequiredTriggers:     6,
			TriggerWindowMinutes: 10,
			ProximityThreshold:   300, // 300 pixels is roughly 15% of a 1080p screen
			AlertWindowStart:     "22:15",
			AlertWindowEnd:       "05:00",
			CooldownMinutes:      60,
			OverrideMinutes:      60,
			ListenPort:           8090,
			PBXHost:              "192.168.40.100",
			PBXUser:              "fsbhoa",
			PBXExtension:         "701",
			CallerID:             "16618730580",
			PlaybackAudio:        "custom/pool-intrusion-alert",
			MQTTHost:             "127.0.0.1",
		}
		saveConfig()
	} else {
		if err := json.Unmarshal(configFile, &appConfig); err != nil {
			log.Fatalf("Error parsing config.json: %v", err)
		}
	}

	http.HandleFunc("/config", handleConfig)

	http.HandleFunc("/override", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		mins := appConfig.OverrideMinutes
		overrideUntil = time.Now().Add(time.Duration(mins) * time.Minute)
		recentTriggers = nil // Clear any pending triggers so they don't fire right after override
		mu.Unlock()

		log.Printf("[OVERRIDE] Manual override activated. Alarms paused for %d minutes.", mins)
		fmt.Fprintf(w, "Alarms paused until %s", overrideUntil.Format("03:04 PM"))
	})

	http.HandleFunc("/resume", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		overrideUntil = time.Time{}
		mu.Unlock()

		log.Printf("[OVERRIDE] Manual override cancelled. Alarms re-enabled.")
		fmt.Fprintf(w, "Alarms re-enabled.")
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/config", http.StatusSeeOther)
			return
		}
		http.NotFound(w, r)
	})

	log.Printf("Loaded Config: Min Updates=%d, Requires %d triggers in %d mins", appConfig.MinUpdates, appConfig.RequiredTriggers, appConfig.TriggerWindowMinutes)

	go startMQTTListener()

	log.Printf("Starting FSBHOA Configuration Portal on port %d...", appConfig.ListenPort)
	addr := fmt.Sprintf(":%d", appConfig.ListenPort)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}

func startMQTTListener() {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("tcp://%s:1883", appConfig.MQTTHost))
	opts.SetClientID("pbx-listener-service")

	opts.OnConnect = func(client mqtt.Client) {
		log.Printf("Connected to MQTT Broker at %s. Subscribing to frigate/events...", appConfig.MQTTHost)
		client.Subscribe("frigate/events", 0, func(client mqtt.Client, msg mqtt.Message) {
			var event FrigateEvent
			if err := json.Unmarshal(msg.Payload(), &event); err == nil {
				processFrigateEvent(event)
			}
		})
	}

	client := mqtt.NewClient(opts)
	for {
		if token := client.Connect(); token.Wait() && token.Error() != nil {
			log.Printf("Failed to connect to MQTT at %s: %v. Retrying in 5s...", appConfig.MQTTHost, token.Error())
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}
}

func processFrigateEvent(event FrigateEvent) {
	if event.After.Label != "person" {
		return
	}

	// We ignore END events completely and rely on our own time/distance math
	if event.Type == "end" {
		return
	}

	mu.Lock()
	defer mu.Unlock()

	now := time.Now()

	// Periodically clean up old objects from memory
	for id, s := range objectStates {
		if now.Sub(s.LastSeen) > 10*time.Second {
			delete(objectStates, id)
		}
	}

	var centerX, centerY float64
	if len(event.After.Box) == 4 {
		ymin, xmin, ymax, xmax := event.After.Box[0], event.After.Box[1], event.After.Box[2], event.After.Box[3]
		centerX = xmin + (xmax-xmin)/2.0
		centerY = ymin + (ymax-ymin)/2.0
	}

	objID := event.After.ID
	state, exists := objectStates[objID]

	if exists {
		// Standard continuation of an existing Frigate ID
		state.HitCount++
		state.LastX = centerX
		state.LastY = centerY
		state.LastSeen = now
	} else {
		// Frigate assigned a NEW ID. Let's see if we can bridge it to an orphaned track based on proximity!
		bridged := false
		for oldID, oldState := range objectStates {
			timeSinceLastSeen := now.Sub(oldState.LastSeen)

			// Is it orphaned? (Not updated in 100ms, but still within 2 seconds)
			if timeSinceLastSeen > 100*time.Millisecond && timeSinceLastSeen <= 2*time.Second {
				dist := math.Sqrt(math.Pow(centerX-oldState.LastX, 2) + math.Pow(centerY-oldState.LastY, 2))

				if dist <= float64(appConfig.ProximityThreshold) {
					// It's a match! Inherit the state.
					state = &ObjectState{
						HitCount: oldState.HitCount + 1,
						LastX:    centerX,
						LastY:    centerY,
						LastSeen: now,
					}
					objectStates[objID] = state
					delete(objectStates, oldID)
					bridged = true
					log.Printf("[BRIDGE] Merged ID due to proximity. Distance: %.0fpx. Current frames: %d", dist, state.HitCount)
					break
				}
			}
		}

		if !bridged {
			// Truly a new person
			state = &ObjectState{
				HitCount: 1,
				LastX:    centerX,
				LastY:    centerY,
				LastSeen: now,
			}
			objectStates[objID] = state
		}
	}

	// TRIGGER LOGIC
	if state.HitCount >= appConfig.MinUpdates {
		// Reset the hit count so this person can generate another trigger if they stay in the pool
		state.HitCount = 0

		recentTriggers = append(recentTriggers, now)

		// Prune triggers that are older than the allowed window
		cutoff := now.Add(-time.Duration(appConfig.TriggerWindowMinutes) * time.Minute)
		var validTriggers []time.Time
		for _, t := range recentTriggers {
			if t.After(cutoff) {
				validTriggers = append(validTriggers, t)
			}
		}
		recentTriggers = validTriggers

		log.Printf("[TRACKING] Solid track verified on %s. Trigger count: %d of %d within %d mins.", event.After.Camera, len(recentTriggers), appConfig.RequiredTriggers, appConfig.TriggerWindowMinutes)

		if len(recentTriggers) >= appConfig.RequiredTriggers {
			evaluateAlarmAndCall(event.After.Camera, now)
		}
	}
}

func evaluateAlarmAndCall(camera string, now time.Time) {
	// 1. Check if the manual override is active
	if now.Before(overrideUntil) {
		log.Printf("[THROTTLED] Threshold met on %s, but manual override is active until %s.", camera, overrideUntil.Format("03:04 PM"))
		recentTriggers = nil
		return
	}

	// 2. Check if we are inside the active Time Window
	currentMins := now.Hour()*60 + now.Minute()
	startParts := strings.Split(appConfig.AlertWindowStart, ":")
	endParts := strings.Split(appConfig.AlertWindowEnd, ":")

	startMins, endMins := 0, 0
	if len(startParts) == 2 && len(endParts) == 2 {
		sh, _ := strconv.Atoi(startParts[0])
		sm, _ := strconv.Atoi(startParts[1])
		startMins = sh*60 + sm

		eh, _ := strconv.Atoi(endParts[0])
		em, _ := strconv.Atoi(endParts[1])
		endMins = eh*60 + em
	}

	isAlertTime := false
	if startMins > endMins {
		if currentMins >= startMins || currentMins < endMins {
			isAlertTime = true
		}
	} else {
		if currentMins >= startMins && currentMins < endMins {
			isAlertTime = true
		}
	}

	// 3. Dispatch call or throttle
	if isAlertTime {
		if time.Since(lastCallTime) < time.Duration(appConfig.CooldownMinutes)*time.Minute {
			log.Printf("[THROTTLED] Threshold met on %s, but system is in a %d-minute cooldown period.", camera, appConfig.CooldownMinutes)
			recentTriggers = nil
			return
		}

		log.Printf("[ALERT] Threshold met (%d triggers)! Initiating PBX alarm...", appConfig.RequiredTriggers)
		lastCallTime = time.Now()
		recentTriggers = nil
		go triggerPBXCall(camera)

	} else {
		log.Printf("[IGNORED] Threshold met on %s, but outside of active alert window (Current time: %02d:%02d).", camera, now.Hour(), now.Minute())
		recentTriggers = nil
	}
}

func triggerPBXCall(camera string) {
	log.Printf("Initiating PBX call to extension %s via %s@%s...", appConfig.PBXExtension, appConfig.PBXUser, appConfig.PBXHost)

	callFileContents := fmt.Sprintf(
		"Channel: Local/%s@from-internal\nCallerID: \"Pool Alarm\" <%s>\nMaxRetries: 2\nRetryTime: 60\nWaitTime: 30\nApplication: Playback\nData: silence/1&%s\n",
		appConfig.PBXExtension,
		appConfig.CallerID,
		appConfig.PlaybackAudio,
	)

	sshCommand := fmt.Sprintf("echo '%s' > /tmp/pool_alarm.call && sudo chown asterisk:asterisk /tmp/pool_alarm.call && sudo mv /tmp/pool_alarm.call /var/spool/asterisk/outgoing/", callFileContents)

	cmd := exec.Command("ssh", "-o", "StrictHostKeyChecking=no", fmt.Sprintf("%s@%s", appConfig.PBXUser, appConfig.PBXHost), sshCommand)

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Failed to trigger PBX call: %v\nOutput: %s", err, string(output))
	} else {
		log.Printf("PBX call successfully dispatched to spool.")
	}
}

func saveConfig() {
	data, _ := json.MarshalIndent(appConfig, "", "  ")
	os.WriteFile("config.json", data, 0644)
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		r.ParseForm()

		mu.Lock()
		appConfig.MinUpdates, _ = strconv.Atoi(r.FormValue("min_updates"))
		appConfig.RequiredTriggers, _ = strconv.Atoi(r.FormValue("required_triggers"))
		appConfig.TriggerWindowMinutes, _ = strconv.Atoi(r.FormValue("trigger_window_minutes"))
		appConfig.ProximityThreshold, _ = strconv.Atoi(r.FormValue("proximity_threshold"))
		appConfig.AlertWindowStart = r.FormValue("start_time")
		appConfig.AlertWindowEnd = r.FormValue("end_time")
		appConfig.CooldownMinutes, _ = strconv.Atoi(r.FormValue("cooldown_minutes"))
		appConfig.OverrideMinutes, _ = strconv.Atoi(r.FormValue("override_minutes"))
		appConfig.ListenPort, _ = strconv.Atoi(r.FormValue("listen_port"))
		appConfig.PBXHost = r.FormValue("pbx_host")
		appConfig.PBXUser = r.FormValue("pbx_user")
		appConfig.PBXExtension = r.FormValue("pbx_extension")
		appConfig.CallerID = r.FormValue("caller_id")
		appConfig.PlaybackAudio = r.FormValue("playback_audio")
		appConfig.MQTTHost = r.FormValue("mqtt_host")
		saveConfig()
		mu.Unlock()

		http.Redirect(w, r, "/config", http.StatusSeeOther)
		return
	}

	mu.Lock()
	data := struct {
		Config          AppConfig
		ActiveOverride  bool
		OverrideTimeStr string
	}{
		Config:          appConfig,
		ActiveOverride:  overrideUntil.After(time.Now()),
		OverrideTimeStr: overrideUntil.Format("03:04 PM"),
	}
	mu.Unlock()

	tmpl, err := template.New("config").Parse(configHTML)
	if err != nil {
		http.Error(w, "Failed to load template", http.StatusInternalServerError)
		return
	}

	tmpl.Execute(w, data)
}
