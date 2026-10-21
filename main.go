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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

//go:embed config.html
var configHTML string

// Global weather state
var (
	isStormy      bool
	weatherReason string
	weatherSource string // "" until the first successful reading
	weatherMutex  sync.RWMutex
)

// WUResponse matches the Weather Underground JSON structure
type WUResponse struct {
	Observations []struct {
		Imperial struct {
			WindGust   float64 `json:"windGust"`
			PrecipRate float64 `json:"precipRate"` // Inches per hour currently falling
		} `json:"imperial"`
	} `json:"observations"`
}

// NWSResponse matches the api.weather.gov latest observation JSON (wind in km/h)
type NWSResponse struct {
	Properties struct {
		Timestamp       time.Time `json:"timestamp"`
		TextDescription string    `json:"textDescription"`
		PresentWeather  []struct {
			Intensity string `json:"intensity"`
			Weather   string `json:"weather"`
		} `json:"presentWeather"`
		WindGust struct {
			Value *float64 `json:"value"`
		} `json:"windGust"`
		WindSpeed struct {
			Value *float64 `json:"value"`
		} `json:"windSpeed"`
	} `json:"properties"`
}

// WeatherObs is one reading from whichever weather source answered
type WeatherObs struct {
	Source     string
	WindGust   float64 // mph
	PrecipRate float64 // in/hr (WU only)
	Raining    bool
	Detail     string
}

// tmpl safely compiles our embedded HTML to prevent CSS formatting conflicts
var tmpl = template.Must(template.New("config").Parse(configHTML))

// AppConfig holds the adjustable thresholds
type AppConfig struct {
	SilentMode           bool   `json:"silent_mode"`
	MinUpdates           int    `json:"min_updates"`
	RequiredTriggers     int    `json:"required_triggers"`
	TriggerWindowMinutes int    `json:"trigger_window_minutes"`
	ProximityThreshold   int    `json:"proximity_threshold"`
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
	WUStationID          string  `json:"wu_station_id"`
	WUAPIKey             string  `json:"wu_api_key"`
	NWSStationID         string  `json:"nws_station_id"`
	WindThreshold        float64 `json:"wind_threshold"`
	LogFilterMode        int    `json:"log_filter_mode"` // 0=Full, 1=Triggers, 2=Alarms
}

// FrigateEvent represents the incoming JSON payload from Frigate over MQTT
type FrigateEvent struct {
	Type  string `json:"type"`
	After struct {
		ID       string    `json:"id"`
		Label    string    `json:"label"`
		Camera   string    `json:"camera"`
		TopScore float64   `json:"top_score"`
		Area     float64   `json:"area"`
		Box      []float64 `json:"box"`
	} `json:"after"`
}

// ObjectState tracks the last known physical position of a person
type ObjectState struct {
	SampleCount int       // Length of the track from Frigate
	TotalConf   float64   // Sum of all confidence scores
	HitsAwarded int       // How many trigger hits this track has generated
	LastSeen    time.Time
	LastX       float64
	LastY       float64
	StartTime   time.Time // To calculate total duration
}

// LogBuffer captures the last 5000 log lines to display in the web UI
type LogBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (lb *LogBuffer) Write(p []byte) (n int, err error) {
	lb.mu.Lock()

	// Create our custom 12-hour AM/PM timestamp
	ts := time.Now().Format("2006/01/02 03:04:05 PM")
	line := fmt.Sprintf("%s %s", ts, string(p))

	lb.lines = append(lb.lines, line)
	if len(lb.lines) > 5000 {
		lb.lines = lb.lines[1:]
	}
	lb.mu.Unlock()

	os.Stdout.Write([]byte(line))
	return len(p), nil // Fulfill the io.Writer contract
}

// Dynamic score cache for Frigate API integration
var (
	cameraMinScores = make(map[string]float64)
	globalMinScore  = 0.4 // Safe fallback if API is unreachable
)

func init() {
	// Runs in the background on startup to prevent blocking or race conditions
	go func() {
		for {
			if loadFrigateConfigOnce() {
				log.Printf("[CONFIG] Successfully synchronized min_scores from Frigate API.")
				return
			}
			log.Printf("[CONFIG] Frigate API not ready yet. Retrying config sync in 5 seconds...")
			time.Sleep(5 * time.Second)
		}
	}()
}

func loadFrigateConfigOnce() bool {
	client := http.Client {
		Timeout: 3 * time.Second,
	}
	resp, err := client.Get("http://127.0.0.1:5000/api/config")
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return false
	}

	mu.Lock()
	defer mu.Unlock()

	// 1. Extract global min_score for person
	if objs, ok := raw["objects"].(map[string]any); ok {
		if filters, ok := objs["filters"].(map[string]any); ok {
			if person, ok := filters["person"].(map[string]any); ok {
				if ms, ok := person["min_score"].(float64); ok {
					globalMinScore = ms
				}
			}
		}
	}

	// 2. Extract per-camera min_score overrides
	if cams, ok := raw["cameras"].(map[string]any); ok {
		for camName, camData := range cams {
			if camMap, ok := camData.(map[string]any); ok {
				if objs, ok := camMap["objects"].(map[string]any); ok {
					if filters, ok := objs["filters"].(map[string]any); ok {
						if person, ok := filters["person"].(map[string]any); ok {
							if ms, ok := person["min_score"].(float64); ok { // Fixed type
								cameraMinScores[camName] = ms
							}
						}
					}
				}
			}
		}
	}
	log.Printf("[CONFIG] Loaded Frigate min_scores. Global: %.2f, Per-Camera overrides: %v", globalMinScore, cameraMinScores)
	return true
}

func getMinScoreForCamera(camera string) float64 {
	mu.Lock()
	defer mu.Unlock()
	if score, exists := cameraMinScores[camera]; exists {
		return score
	}
	return globalMinScore
}

var (
	objectStates   = make(map[string]*ObjectState)
	recentTriggers []time.Time
	lastCallTime   time.Time
	overrideUntil  time.Time
	mu             sync.Mutex
	appConfig      AppConfig
	logBuf         LogBuffer
)

func main() {
	log.SetOutput(&logBuf)
	log.SetFlags(0)

	// Load configuration from file
	configFile, err := os.ReadFile("config.json")
	if err != nil {
		log.Println("config.json not found, creating default configuration...")
		appConfig = AppConfig{
			SilentMode:           true, // Defaulting to true to save your sleep!
			MinUpdates:           3,
			RequiredTriggers:     6,
			TriggerWindowMinutes: 10,
			ProximityThreshold:   300,
			AlertWindowStart:     "22:15",
			AlertWindowEnd:       "05:00",
			CooldownMinutes:      60,
			OverrideMinutes:      60,
			ListenPort:           8091,
			PBXHost:              "192.168.40.100",
			PBXUser:              "fsbhoa",
			PBXExtension:         "701",
			CallerID:             "16618730580",
			PlaybackAudio:        "custom/pool-intrusion-alert",
			MQTTHost:             "127.0.0.1",
			WUStationID:          "KCABAKER235",
			NWSStationID:         "KBFL",
			WindThreshold:        15.0,
			LogFilterMode:        0,
		}
		if err := saveConfig(); err != nil {
			log.Printf("[CONFIG] Failed to write default config.json: %v", err)
		}
	} else {
		if err := json.Unmarshal(configFile, &appConfig); err != nil {
			log.Fatalf("Error parsing config.json: %v", err)
		}
	}
	// Keep running on a bad config (the alarm must stay up), but make the problems visible.
	for _, p := range validateConfig(appConfig) {
		log.Printf("[CONFIG] Problem in config.json: %s", p)
	}

	http.HandleFunc("/config", handleConfig)

	http.HandleFunc("/toggle-silent", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		appConfig.SilentMode = !appConfig.SilentMode
		saveErr := saveConfig()
		isSilent := appConfig.SilentMode
		mu.Unlock()
		if saveErr != nil {
			log.Printf("[CONFIG] Failed to save silent mode to config.json (change is active until restart): %v", saveErr)
		}

		log.Printf("[SILENT MODE] Toggled via Web UI. SilentMode is now: %v", isSilent)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(map[string]bool{"silent_mode": isSilent})
	})

	http.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
		logBuf.mu.Lock()
		linesCopy := make([]string, len(logBuf.lines))
		copy(linesCopy, logBuf.lines)
		logBuf.mu.Unlock()

		mu.Lock()
		mode := appConfig.LogFilterMode
		mu.Unlock()

		var filtered []string
		for _, line := range linesCopy {
			if mode == 1 {
				// Mode 1: Triggers & Alarms
				if strings.Contains(line, "[TRACKING]") || strings.Contains(line, "[ALERT]") ||
				   strings.Contains(line, "[SILENT MODE]") || strings.Contains(line, "[IGNORED]") ||
				   strings.Contains(line, "[THROTTLED]") || strings.Contains(line, "[OVERRIDE]") ||
				   strings.Contains(line, "[TEST]") || strings.Contains(line, "[WEATHER]") {
					filtered = append(filtered, line)
				}
			} else if mode == 2 {
				// Mode 2: Alarms Only (strips out the [TRACKING] lines)
				if strings.Contains(line, "[ALERT]") || strings.Contains(line, "[SILENT MODE]") ||
				   strings.Contains(line, "[IGNORED]") || strings.Contains(line, "[THROTTLED]") ||
				   strings.Contains(line, "[OVERRIDE]") || strings.Contains(line, "[TEST]") ||
				   strings.Contains(line, "[WEATHER]") {
					filtered = append(filtered, line)
				}
			} else {
				// Mode 0: Full Log
				filtered = append(filtered, line)
			}
		}

		// Friendly message if the filtered list is empty so the screen doesn't just go blank
		if len(filtered) == 0 {
			if mode == 1 {
				filtered = append(filtered, "--- Filtering for Triggers & Alarms. Waiting for events... ---")
			} else if mode == 2 {
				filtered = append(filtered, "--- Filtering for Alarms Only. Waiting for events... ---")
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(filtered)
	})

	http.HandleFunc("/pause", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		overrideUntil = time.Now().Add(time.Duration(appConfig.OverrideMinutes) * time.Minute)
		mu.Unlock()
		log.Printf("[OVERRIDE] Alarms paused via web request for %d minutes.", appConfig.OverrideMinutes)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprintf(w, "Alarms paused until %s", overrideUntil.Format("03:04:05 PM"))
	})

	http.HandleFunc("/override", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		// Set the override to exactly 10 years in the future to make it permanent
		overrideUntil = time.Now().AddDate(10, 0, 0) 
		mu.Unlock()
		log.Printf("[OVERRIDE] Alarms disabled PERMANENTLY via web request.")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprintf(w, "Alarms permanently disabled until a /resume command is received.")
	})

	http.HandleFunc("/resume", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		overrideUntil = time.Time{}
		mu.Unlock()
		log.Printf("[OVERRIDE] Alarms resumed via web request.")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprintf(w, "Alarms resumed and active.")
	})

	http.HandleFunc("/test-alarm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		log.Printf("[TEST] Manual PBX test triggered via web UI.")

		// Call the exact same function the real alarm uses, passing a dummy camera name
		mu.Lock()
		cfg := appConfig
		mu.Unlock()
		go triggerPBXCall("Web_Test_Button", cfg)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprintf(w, `{"status": "success", "message": "PBX call sent to spool!"}`)
	})

	http.HandleFunc("/weather-status", func(w http.ResponseWriter, r *http.Request) {
               weatherMutex.RLock()
               status := map[string]interface{}{
                   "isStormy": isStormy,
                   "reason":   weatherReason,
                   "source":   weatherSource,
               }
               weatherMutex.RUnlock()

               w.Header().Set("Content-Type", "application/json")
               json.NewEncoder(w).Encode(status)
        })

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/config", http.StatusSeeOther)
			return
		}
		http.NotFound(w, r)
	})


	log.Printf("Loaded Config: Min Updates=%d, Requires %d triggers in %d mins", appConfig.MinUpdates, appConfig.RequiredTriggers, appConfig.TriggerWindowMinutes)

	go monitorWeather()
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

	// Enable automatic reconnection and connection monitoring
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)

	opts.OnConnect = func(client mqtt.Client) {
		topic := "frigate/events"
		log.Printf("Connected to MQTT Broker at %s. Subscribing to %s", appConfig.MQTTHost, topic)
		if token := client.Subscribe(topic, 1, func(client mqtt.Client, msg mqtt.Message) {
			// RAW DEBUG: This prints the exact second an MQTT message arrives,
			// regardless of whether it's valid JSON or not.
			//log.Printf("DEBUG MQTT RECEIVED -> Topic: %s | Size: %d bytes", msg.Topic(), len(msg.Payload()))

			var event FrigateEvent
			if err := json.Unmarshal(msg.Payload(), &event); err != nil {
				// Log the error so we can see if malformed data comes through
				log.Printf("JSON unmarshal error: %v | Payload: %s", err, string(msg.Payload()))
				return
			}
			processFrigateEvent(event)
		}); token.Wait() && token.Error() != nil {
			log.Printf("Subscription failed: %v", token.Error())
		}
	}
	opts.SetConnectionLostHandler(func(client mqtt.Client, err error) {
		log.Printf("[MQTT ERROR] Connection lost: %v. Attempting to reconnect...", err)
	})

	// Initial connection attempt
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
	// TRACE: Print out what the event actually contains
	//log.Printf("[EVENT TRACE] Camera: %s | Label: %s | Score: %.2f",
	//	event.After.Camera, event.After.Label, event.After.TopScore)

	if event.After.Label != "person" {
		return
	}

	if event.Type == "end" {
		mu.Lock()
		// Print a clean summary of the track before deleting it
		if state, exists := objectStates[event.After.ID]; exists {
			duration := time.Since(state.StartTime).Round(time.Second)
			avgConf := state.TotalConf / float64(state.SampleCount)
			log.Printf("[TRACK END] %s track on %s ended | Duration: %s | Samples: %d | Avg Conf: %.2f | Hits Generated: %d",
				event.After.Label, event.After.Camera, duration, state.SampleCount, avgConf, state.HitsAwarded)
			delete(objectStates, event.After.ID)
		}
		mu.Unlock()
		return
	}

	minScore := getMinScoreForCamera(event.After.Camera)
	conf := event.After.TopScore
	if conf < minScore {
		conf = minScore
	}

	mu.Lock()
	defer mu.Unlock()

	now := time.Now()

	for id, s := range objectStates {
		if now.Sub(s.LastSeen) > 60*time.Second {
			delete(objectStates, id)
		}
	}

	var centerX, centerY, width, height float64
	if len(event.After.Box) == 4 {
		ymin, xmin, ymax, xmax := event.After.Box[0], event.After.Box[1], event.After.Box[2], event.After.Box[3]
		centerX = xmin + (xmax-xmin)/2.0
		centerY = ymin + (ymax-ymin)/2.0
		width = xmax - xmin
		height = ymax - ymin
	}
	// === DATA GATHERING TELEMETRY ===
	log.Printf("[TELEMETRY] Cam: %s | ID: %s | Conf: %.2f | Area: %.0f | WxH: %.0fx%.0f | Center: %.0f,%.0f",
		event.After.Camera, event.After.ID, conf, event.After.Area, 
		width, height, centerX, centerY)

	objID := event.After.ID
	state, exists := objectStates[objID]

	if exists {
		state.SampleCount++
		state.TotalConf += conf
		state.LastX = centerX
		state.LastY = centerY
		state.LastSeen = now
	} else {
		bridged := false
		for oldID, oldState := range objectStates {
			timeSinceLastSeen := now.Sub(oldState.LastSeen)

			if timeSinceLastSeen > 100*time.Millisecond && timeSinceLastSeen <= 3*time.Second {
				dist := math.Sqrt(math.Pow(centerX-oldState.LastX, 2) + math.Pow(centerY-oldState.LastY, 2))

				if dist <= float64(appConfig.ProximityThreshold) {
					state = &ObjectState{
						SampleCount: oldState.SampleCount + 1,
						TotalConf:   oldState.TotalConf + conf,
						HitsAwarded: oldState.HitsAwarded,
						LastX:       centerX,
						LastY:       centerY,
						LastSeen:    now,
						StartTime:   oldState.StartTime,
					}
		                        log.Printf("[BRIDGED] Active track on %s to %s", 
					    oldID, objID)
					objectStates[objID] = state
					delete(objectStates, oldID)
					bridged = true
					break
				}
			}
		}

		if !bridged {
			state = &ObjectState{
				SampleCount: 1,
				TotalConf:   conf,
				HitsAwarded: 0,
				LastX:       centerX,
				LastY:       centerY,
				LastSeen:    now,
				StartTime:   now,
			}
			objectStates[objID] = state
		}
	}

	// === METRICS EVALUATION ===
	// Length (SampleCount) and Confidence (TotalConf) work together.
	// For every block of "MinUpdates" worth of confidence accumulated, award 1 hit.
	minUpdates := appConfig.MinUpdates
	if minUpdates < 1 {
		minUpdates = 1 // 0 would divide by zero and silently stop all alarms
	}
	earnedHits := int(state.TotalConf / float64(minUpdates))

	// If this track grew long/confident enough to earn a new hit...
	if earnedHits > state.HitsAwarded {
		newHits := earnedHits - state.HitsAwarded
		state.HitsAwarded = earnedHits

		for i := 0; i < newHits; i++ {
			recentTriggers = append(recentTriggers, now)
		}

		cutoff := now.Add(-time.Duration(appConfig.TriggerWindowMinutes) * time.Minute)
		var validTriggers []time.Time
		for _, t := range recentTriggers {
			if t.After(cutoff) {
				validTriggers = append(validTriggers, t)
			}
		}
		recentTriggers = validTriggers

		// Calculate display metrics for the log
		// Convert 0.85 to 85 for the percentage display
		avgConfPct := int((state.TotalConf / float64(state.SampleCount)) * 100)

		// Get the duration as a clean decimal (e.g., 1.5)
		trackSecs := now.Sub(state.StartTime).Seconds()

		log.Printf("[TRACKING] Active track on %s, Avg Conf: %d%% for %.1f sec; hit %d of %d within %d mins.",
			event.After.Camera, avgConfPct, trackSecs, len(recentTriggers), appConfig.RequiredTriggers, appConfig.TriggerWindowMinutes)

		if len(recentTriggers) >= appConfig.RequiredTriggers {
			evaluateAlarmAndCall(event.After.Camera, now)
		}
	}
}

func evaluateAlarmAndCall(camera string, now time.Time) {
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

	if isAlertTime {
		if time.Now().Before(overrideUntil) {
			log.Printf("[OVERRIDE] Threshold met on %s, but system is currently paused.", camera)
			recentTriggers = nil
			return
		}

		// WEATHER LOCKOUT CHECK (rain or high wind)
		weatherMutex.RLock()
		stormy, reason := isStormy, weatherReason
		weatherMutex.RUnlock()
		if stormy {
			log.Printf("[WEATHER] Threshold met on %s, but alarms are auto-muted: %s.", camera, reason)
			recentTriggers = nil
			return
		}

		if time.Since(lastCallTime) < time.Duration(appConfig.CooldownMinutes)*time.Minute {
			log.Printf("[THROTTLED] Threshold met on %s, but system is in a %d-minute cooldown period.", camera, appConfig.CooldownMinutes)
			recentTriggers = nil
			return
		}

		// SILENT MODE CHECK
		if appConfig.SilentMode {
			log.Printf("[SILENT MODE] Threshold met (%d triggers) on %s! Would have called PBX, but system is muted.", appConfig.RequiredTriggers, camera)
			lastCallTime = time.Now()
			recentTriggers = nil
			return
		}

		log.Printf("[ALERT] Threshold met (%d triggers)! Initiating PBX alarm...", appConfig.RequiredTriggers)
		lastCallTime = time.Now()
		recentTriggers = nil
		go triggerPBXCall(camera, appConfig) // caller holds mu; pass a copy

	} else {
		log.Printf("[IGNORED] Threshold met on %s, but outside of active alert window (Current time: %s).", camera, now.Format("03:04 PM"))
		recentTriggers = nil
	}
}

// The call file is written on the PBX by this fixed command; its contents arrive on stdin,
// so no config value is ever interpreted by a shell.
const pbxSpoolCommand = "cat > /tmp/pool_alarm.call && sudo chown asterisk:asterisk /tmp/pool_alarm.call && sudo mv /tmp/pool_alarm.call /var/spool/asterisk/outgoing/"

func triggerPBXCall(camera string, cfg AppConfig) {
	if problems := validatePBXFields(cfg); len(problems) > 0 {
		log.Printf("[ALERT] PBX call NOT placed, invalid PBX settings: %s", strings.Join(problems, "; "))
		return
	}
	log.Printf("Initiating PBX call to extension %s via %s@%s...", cfg.PBXExtension, cfg.PBXUser, cfg.PBXHost)

	callFileContents := fmt.Sprintf(
		"Channel: Local/%s@from-internal\nCallerID: \"Pool Alarm\" <%s>\nMaxRetries: 2\nRetryTime: 60\nWaitTime: 30\nApplication: Playback\nData: silence/2&%s&silence/2&%s&silence/2&%s&silence/2&%s\n",
		cfg.PBXExtension,
		cfg.CallerID,
		cfg.PlaybackAudio, // Plays 1st time
		cfg.PlaybackAudio, // Plays 2nd time
		cfg.PlaybackAudio, // Plays 3rd time
		cfg.PlaybackAudio, // Plays 4th time
	)

	cmd := exec.Command("ssh",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new", // still refuses a changed PBX host key
		"-o", "ConnectTimeout=15",
		"--", cfg.PBXUser+"@"+cfg.PBXHost, pbxSpoolCommand)
	cmd.Stdin = strings.NewReader(callFileContents)

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Failed to trigger PBX call: %v\nOutput: %s", err, string(output))
	} else {
		log.Printf("PBX call successfully dispatched to spool.")
	}
}

var (
	digitsRe  = regexp.MustCompile(`^[0-9]{1,20}$`)
	audioRe   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_/-]*$`)
	hostRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	userRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	alnumRe   = regexp.MustCompile(`^[A-Za-z0-9]*$`)
	hhmmRe    = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

// validatePBXFields checks the settings that end up in the ssh command line and the call file.
func validatePBXFields(c AppConfig) []string {
	var p []string
	if !hostRe.MatchString(c.PBXHost) {
		p = append(p, "PBX host must be a hostname or IP address")
	}
	if !userRe.MatchString(c.PBXUser) {
		p = append(p, "PBX user may only contain letters, digits, '_', '.', '-'")
	}
	if !digitsRe.MatchString(c.PBXExtension) {
		p = append(p, "PBX extension must be digits only")
	}
	if !digitsRe.MatchString(c.CallerID) {
		p = append(p, "Caller ID must be digits only")
	}
	if !audioRe.MatchString(c.PlaybackAudio) || strings.Contains(c.PlaybackAudio, "..") {
		p = append(p, "Playback audio must be a sound name like custom/pool_alarm")
	}
	return p
}

// validateConfig returns every problem with c; an empty result means it is safe to use.
func validateConfig(c AppConfig) []string {
	p := validatePBXFields(c)
	if c.MinUpdates < 1 {
		p = append(p, "Min updates must be at least 1")
	}
	if c.RequiredTriggers < 1 {
		p = append(p, "Required triggers must be at least 1")
	}
	if c.TriggerWindowMinutes < 1 {
		p = append(p, "Trigger window must be at least 1 minute")
	}
	if c.ProximityThreshold < 0 {
		p = append(p, "Proximity threshold cannot be negative")
	}
	if !hhmmRe.MatchString(c.AlertWindowStart) || !hhmmRe.MatchString(c.AlertWindowEnd) {
		p = append(p, "Alert window times must be HH:MM")
	}
	if c.CooldownMinutes < 0 {
		p = append(p, "Cooldown cannot be negative")
	}
	if c.OverrideMinutes < 1 {
		p = append(p, "Pause duration must be at least 1 minute")
	}
	if c.ListenPort < 1024 || c.ListenPort > 65535 {
		p = append(p, "Listener port must be between 1024 and 65535")
	}
	if !hostRe.MatchString(c.MQTTHost) {
		p = append(p, "MQTT host must be a hostname or IP address")
	}
	if !alnumRe.MatchString(c.WUStationID) || !alnumRe.MatchString(c.NWSStationID) {
		p = append(p, "Weather station IDs may only contain letters and digits")
	}
	if !alnumRe.MatchString(c.WUAPIKey) {
		p = append(p, "Weather Underground API key may only contain letters and digits")
	}
	if c.WindThreshold <= 0 {
		p = append(p, "Wind gust threshold must be greater than 0")
	}
	if c.LogFilterMode < 0 || c.LogFilterMode > 2 {
		p = append(p, "Log filter must be 0, 1 or 2")
	}
	return p
}

// saveConfig writes appConfig atomically (temp file + rename) so a failed write never truncates config.json.
func saveConfig() error {
	data, err := json.MarshalIndent(appConfig, "", "  ")
	if err != nil {
		return err
	}
	tmp := "config.json.tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, "config.json")
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		r.ParseForm()

		mu.Lock()
		cfg := appConfig
		mu.Unlock()

		var problems []string
		atoi := func(field, label string) int {
			v, err := strconv.Atoi(strings.TrimSpace(r.FormValue(field)))
			if err != nil {
				problems = append(problems, label+" must be a whole number")
			}
			return v
		}
		cfg.MinUpdates = atoi("min_updates", "Min updates")
		cfg.RequiredTriggers = atoi("required_triggers", "Required triggers")
		cfg.TriggerWindowMinutes = atoi("trigger_window_minutes", "Trigger window")
		cfg.ProximityThreshold = atoi("proximity_threshold", "Proximity threshold")
		cfg.AlertWindowStart = strings.TrimSpace(r.FormValue("start_time"))
		cfg.AlertWindowEnd = strings.TrimSpace(r.FormValue("end_time"))
		cfg.CooldownMinutes = atoi("cooldown_minutes", "Cooldown")
		cfg.OverrideMinutes = atoi("override_minutes", "Pause duration")
		cfg.ListenPort = atoi("listen_port", "Listener port")
		cfg.PBXHost = strings.TrimSpace(r.FormValue("pbx_host"))
		cfg.PBXUser = strings.TrimSpace(r.FormValue("pbx_user"))
		cfg.PBXExtension = strings.TrimSpace(r.FormValue("pbx_extension"))
		cfg.CallerID = strings.TrimSpace(r.FormValue("caller_id"))
		cfg.PlaybackAudio = strings.TrimSpace(r.FormValue("playback_audio"))
		cfg.WUStationID = strings.TrimSpace(r.FormValue("wu_station_id"))
		cfg.WUAPIKey = strings.TrimSpace(r.FormValue("wu_api_key"))
		cfg.NWSStationID = strings.TrimSpace(r.FormValue("nws_station_id"))
		wind, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("wind_threshold")), 64)
		if err != nil {
			problems = append(problems, "Wind gust threshold must be a number")
		}
		cfg.WindThreshold = wind
		cfg.LogFilterMode = atoi("log_filter_mode", "Log filter")
		cfg.MQTTHost = strings.TrimSpace(r.FormValue("mqtt_host"))

		if len(problems) == 0 {
			problems = validateConfig(cfg)
		}
		if len(problems) > 0 {
			log.Printf("[CONFIG] Settings NOT saved: %s", strings.Join(problems, "; "))
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "<h3>Settings not saved</h3><ul>")
			for _, p := range problems {
				fmt.Fprintf(w, "<li>%s</li>", template.HTMLEscapeString(p))
			}
			fmt.Fprint(w, `</ul><p><a href="javascript:history.back()">Go back and fix</a></p>`)
			return
		}

		mu.Lock()
		cfg.SilentMode = appConfig.SilentMode // toggled separately; keep its current value
		old := appConfig
		appConfig = cfg
		err = saveConfig()
		if err != nil {
			appConfig = old
		}
		mu.Unlock()
		if err != nil {
			log.Printf("[CONFIG] Failed to save config.json: %v", err)
			http.Error(w, "Settings could not be saved: "+err.Error(), http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, "/config", http.StatusSeeOther)
		return
	}

	mu.Lock()
	cfg := appConfig
	mu.Unlock()

	w.Header().Set("Content-Type", "text/html")
	if err := tmpl.Execute(w, cfg); err != nil {
		log.Printf("Error rendering template: %v", err)
	}
}
var weatherClient = &http.Client{Timeout: 15 * time.Second}

func monitorWeather() {
	for {
		// 1. Safely lock and read the latest settings from memory on every loop!
		mu.Lock()
		wuStation := appConfig.WUStationID
		wuKey := appConfig.WUAPIKey
		nwsStation := appConfig.NWSStationID
		threshold := appConfig.WindThreshold
		mu.Unlock()

		if wuStation == "" {
			wuStation = "KCABAKER235" // Fallback just in case
		}
		if nwsStation == "" {
			nwsStation = "KBFL"
		}
		if threshold <= 0 {
			threshold = 15.0 // Safe fallback so 0 doesn't mute alarms forever
		}

		// 2. Prefer the nearby WU station; fall back to NWS when there is no key or WU fails
		var obs WeatherObs
		var err error
		if wuKey != "" {
			obs, err = fetchWU(wuStation, wuKey)
			if err != nil {
				log.Printf("[WEATHER] WU station %s failed: %v. Falling back to NWS %s.", wuStation, err, nwsStation)
			}
		}
		if wuKey == "" || err != nil {
			obs, err = fetchNWS(nwsStation)
		}

		if err != nil {
			log.Printf("[WEATHER] NWS station %s failed: %v. Keeping previous weather state.", nwsStation, err)
		} else {
			stormy := false
			reason := ""
			if obs.WindGust > threshold {
				stormy = true
				reason = fmt.Sprintf("High Winds (%.1f mph gust)", obs.WindGust)
			} else if obs.Raining {
				stormy = true
				reason = "Active Rain (" + obs.Detail + ")"
			}

			// Safely update the global state
			weatherMutex.Lock()
			changed := stormy != isStormy || obs.Source != weatherSource
			isStormy = stormy
			weatherReason = reason
			weatherSource = obs.Source
			weatherMutex.Unlock()

			if changed {
				if stormy {
					log.Printf("[WEATHER] Alarms auto-muted via %s: %s", obs.Source, reason)
				} else {
					log.Printf("[WEATHER] Weather clear via %s (gust %.1f mph, %s). Alarms active.", obs.Source, obs.WindGust, obs.Detail)
				}
			}
		}

		// Polling every 10 minutes keeps us well under any rate limits.
		// Until the first good reading (e.g. DNS not up yet after a reboot), retry every minute.
		weatherMutex.RLock()
		haveReading := weatherSource != ""
		weatherMutex.RUnlock()
		if haveReading {
			time.Sleep(10 * time.Minute)
		} else {
			time.Sleep(1 * time.Minute)
		}
	}
}

// fetchWU reads the current observation from a Weather Underground personal weather station.
func fetchWU(stationID, apiKey string) (WeatherObs, error) {
	url := fmt.Sprintf("https://api.weather.com/v2/pws/observations/current?stationId=%s&format=json&units=e&apiKey=%s", stationID, apiKey)
	resp, err := weatherClient.Get(url)
	if err != nil {
		return WeatherObs{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return WeatherObs{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var wu WUResponse
	if err := json.NewDecoder(resp.Body).Decode(&wu); err != nil {
		return WeatherObs{}, err
	}
	if len(wu.Observations) == 0 {
		return WeatherObs{}, fmt.Errorf("no observations (station offline?)")
	}

	obs := wu.Observations[0].Imperial
	return WeatherObs{
		Source:     "WU " + stationID,
		WindGust:   obs.WindGust,
		PrecipRate: obs.PrecipRate,
		Raining:    obs.PrecipRate > 0.0,
		Detail:     fmt.Sprintf("%.2f in/hr", obs.PrecipRate),
	}, nil
}

// fetchNWS reads the latest observation from a National Weather Service station (no key needed).
func fetchNWS(stationID string) (WeatherObs, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://api.weather.gov/stations/%s/observations/latest", stationID), nil)
	if err != nil {
		return WeatherObs{}, err
	}
	req.Header.Set("User-Agent", "fsbhoa-pool-alarm") // NWS rejects requests without one
	req.Header.Set("Accept", "application/geo+json")

	resp, err := weatherClient.Do(req)
	if err != nil {
		return WeatherObs{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return WeatherObs{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var nws NWSResponse
	if err := json.NewDecoder(resp.Body).Decode(&nws); err != nil {
		return WeatherObs{}, err
	}
	p := nws.Properties
	if age := time.Since(p.Timestamp); age > 2*time.Hour {
		return WeatherObs{}, fmt.Errorf("observation is stale (%s old)", age.Round(time.Minute))
	}

	// Gusts are only reported when present; otherwise use sustained wind. NWS reports km/h.
	windKmh := p.WindSpeed.Value
	if p.WindGust.Value != nil {
		windKmh = p.WindGust.Value
	}
	var gustMph float64
	if windKmh != nil {
		gustMph = *windKmh / 1.609344
	}

	raining := false
	for _, w := range p.PresentWeather {
		if strings.Contains(w.Weather, "rain") || strings.Contains(w.Weather, "drizzle") || strings.Contains(w.Weather, "thunder") {
			raining = true
		}
	}

	detail := p.TextDescription
	if detail == "" {
		detail = "no description"
	}
	return WeatherObs{
		Source:   "NWS " + stationID,
		WindGust: gustMph,
		Raining:  raining,
		Detail:   detail,
	}, nil
}


