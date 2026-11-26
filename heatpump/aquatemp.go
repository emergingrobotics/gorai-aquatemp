// Package heatpump provides a gorai sensor component for Aqua Temp /
// Linked-Go heat pumps.
//
// The component wraps the goaquatemp client library. It polls the Aqua Temp
// cloud on an interval, caches the newest snapshot for Readings(), and, when
// given a NATS connection, publishes:
//
//   - every successful poll as JSON on gorai.<robot_id>.<name>.data
//   - state changes on gorai.<robot_id>.<name>.event:
//     source_down / source_up (API failures, device offline) and
//     state_change (heating, idle, defrosting, ...).
//
// Fields the cloud did not report are left out of the payload, never sent as 0.
// Water temperatures read stagnant pipe water when there is no flow; pool_temp_c
// is published only when the readings can be trusted (see water_temp_status).
package heatpump

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/emergingrobotics/gorai/pkg/registry"
	"github.com/emergingrobotics/gorai/pkg/resource"
	"github.com/nats-io/nats.go"
	aq "github.com/user/goaquatemp"
)

const (
	defaultPollInterval = 2 * time.Minute
	minimumPollInterval = 30 * time.Second
	defaultName         = "heat-pump"

	// failThreshold is how many consecutive failed polls mark the source down.
	failThreshold = 3
)

// Event kinds and reasons published on the .event subject.
const (
	EventSourceDown  = "source_down"
	EventSourceUp    = "source_up"
	EventStateChange = "state_change"

	ReasonAPIError      = "api_error"
	ReasonDeviceOffline = "device_offline"
)

func init() {
	registry.RegisterComponent("sensor", "aquatemp", New)
}

// Config holds parsed and validated configuration for the component.
type Config struct {
	Username     string
	Password     string
	DeviceCode   string
	APIVersion   string
	BaseURL      string
	PollInterval time.Duration
}

// HeatPump is a gorai sensor component backed by the Aqua Temp cloud API.
type HeatPump struct {
	name   resource.Name
	logger *slog.Logger
	now    func() time.Time

	mu         sync.RWMutex // guards config, client, deviceName, readings, lastPollAt, pollErr
	config     *Config
	client     aq.Client
	deviceName string
	readings   map[string]any
	lastPollAt time.Time
	pollErr    error

	pollMu     sync.Mutex // serializes polls; guards the fields below
	errCount   int        // consecutive failed polls
	downReason string     // "" when up
	lastState  aq.OperatingState

	natsConn   *nats.Conn
	topic      string
	eventTopic string

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// clientFactory builds the Aqua Temp client. Tests replace it.
var clientFactory = func(cfg *Config) (aq.Client, error) {
	opts := []aq.Option{
		aq.WithUsername(cfg.Username),
		aq.WithPassword(cfg.Password),
	}
	if cfg.APIVersion != "" {
		opts = append(opts, aq.WithAPIVersion(aq.APIVersion(cfg.APIVersion)))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, aq.WithBaseURL(cfg.BaseURL))
	}
	return aq.NewClient(opts...)
}

// New creates the component, logs in, resolves the device and starts polling.
func New(ctx context.Context, deps registry.Dependencies, conf registry.Config) (any, error) {
	cfg, err := parseConfig(conf)
	if err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	client, err := clientFactory(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating aquatemp client: %w", err)
	}
	devName, err := resolveDevice(ctx, client, cfg)
	if err != nil {
		return nil, err
	}

	var nc *nats.Conn
	if deps != nil {
		if v, err := deps.Get("nats"); err == nil {
			nc, _ = v.(*nats.Conn)
		}
	}

	h := newHeatPump(conf, cfg, client, devName, nc, time.Now)
	if nc != nil {
		h.logger.Info("NATS publishing enabled", "topic", h.topic, "event_topic", h.eventTopic)
	} else {
		h.logger.Warn("NATS connection not available, data will not be published")
	}
	h.startPolling(ctx)
	return h, nil
}

// newHeatPump builds the component without starting the poll loop.
func newHeatPump(conf registry.Config, cfg *Config, client aq.Client, devName string, nc *nats.Conn, now func() time.Time) *HeatPump {
	name, _ := conf["name"].(string)
	if name == "" {
		name = defaultName
	}
	namespace := "gorai"
	if ns, ok := conf["namespace"].(string); ok && ns != "" {
		namespace = ns
	}
	robotID := namespace
	if rid, ok := conf["robot_id"].(string); ok && rid != "" {
		robotID = rid
	}
	return &HeatPump{
		name:       resource.NewComponentName(namespace, "sensor", name),
		logger:     slog.Default().With("component", "sensor", "model", "aquatemp", "name", name),
		now:        now,
		config:     cfg,
		client:     client,
		deviceName: devName,
		natsConn:   nc,
		topic:      fmt.Sprintf("gorai.%s.%s.data", robotID, name),
		eventTopic: fmt.Sprintf("gorai.%s.%s.event", robotID, name),
	}
}

func parseConfig(conf registry.Config) (*Config, error) {
	cfg := &Config{PollInterval: defaultPollInterval}

	str := func(key string) string { s, _ := conf[key].(string); return s }

	if cfg.Username = str("username"); cfg.Username == "" {
		return nil, errors.New("username is required")
	}
	if cfg.Password = str("password"); cfg.Password == "" {
		return nil, errors.New("password is required")
	}
	cfg.DeviceCode = str("device_code")
	cfg.BaseURL = str("base_url")

	switch v := str("api_version"); v {
	case "", string(aq.APILegacy), string(aq.APIHiTemp), string(aq.APIModern):
		cfg.APIVersion = v
	default:
		return nil, fmt.Errorf("invalid api_version %q: want legacy, hitemp or modern", v)
	}

	if s := str("poll_interval"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("invalid poll_interval %q: %w", s, err)
		}
		if d < minimumPollInterval {
			return nil, fmt.Errorf("poll_interval %s is below minimum %s", d, minimumPollInterval)
		}
		cfg.PollInterval = d
	}
	return cfg, nil
}

// resolveDevice logs in and fills cfg.DeviceCode when it is empty and the
// account has exactly one device. It returns the device's display name.
func resolveDevice(ctx context.Context, client aq.Client, cfg *Config) (string, error) {
	if err := client.Login(ctx); err != nil {
		return "", fmt.Errorf("aquatemp login: %w", err)
	}
	devices, err := client.GetDevices(ctx)
	if err != nil {
		return "", fmt.Errorf("listing devices: %w", err)
	}
	if cfg.DeviceCode != "" {
		for _, d := range devices {
			if d.Code == cfg.DeviceCode {
				return d.Name, nil
			}
		}
		return "", fmt.Errorf("device %q not found on account (%d devices)", cfg.DeviceCode, len(devices))
	}
	switch len(devices) {
	case 0:
		return "", errors.New("no devices found on account")
	case 1:
		cfg.DeviceCode = devices[0].Code
		return devices[0].Name, nil
	default:
		return "", fmt.Errorf("multiple devices found (%d), specify device_code in config", len(devices))
	}
}

func (h *HeatPump) startPolling(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	h.cancel = cancel

	h.mu.RLock()
	interval := h.config.PollInterval
	h.mu.RUnlock()

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.poll(ctx)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.poll(ctx)
			}
		}
	}()
}

// poll fetches a snapshot, publishes it and updates source state.
func (h *HeatPump) poll(ctx context.Context) {
	h.pollMu.Lock()
	defer h.pollMu.Unlock()

	h.mu.RLock()
	cfg, client, devName := h.config, h.client, h.deviceName
	h.mu.RUnlock()

	data, err := client.GetDeviceData(ctx, cfg.DeviceCode)
	var status *aq.DeviceStatus
	if err == nil {
		status, err = client.GetDeviceStatus(ctx, cfg.DeviceCode)
	}
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down
		}
		h.errCount++
		h.mu.Lock()
		h.pollErr = err
		h.mu.Unlock()
		h.logger.Error("poll failed", "error", err, "consecutive", h.errCount)
		h.updateSource(cfg, false)
		return
	}
	h.errCount = 0

	r := toReadings(data, status, devName, h.now())
	h.mu.Lock()
	h.readings = r
	h.lastPollAt = h.now()
	h.pollErr = nil
	h.mu.Unlock()

	h.publish(h.topic, r)
	h.logger.Debug("published reading", "state", data.State, "online", status.Online)

	h.updateSource(cfg, status.Online)
	if h.downReason == "" && data.State != h.lastState {
		if h.lastState != "" {
			h.publish(h.eventTopic, map[string]any{
				"ts":     h.now().UTC().Format(time.RFC3339),
				"kind":   EventStateChange,
				"device": cfg.DeviceCode,
				"from":   string(h.lastState),
				"to":     string(data.State),
			})
			h.logger.Info("operating state changed", "from", h.lastState, "to", data.State)
		}
		h.lastState = data.State
	}
}

// updateSource publishes source_down / source_up when the source state
// changes. online is ignored when the poll failed. Must hold pollMu.
func (h *HeatPump) updateSource(cfg *Config, online bool) {
	want := h.downReason
	switch {
	case h.errCount >= failThreshold:
		want = ReasonAPIError
	case h.errCount > 0:
		// Transient failure: keep the current state.
	case !online:
		want = ReasonDeviceOffline
	default:
		want = ""
	}
	if want == h.downReason {
		return
	}
	h.downReason = want

	ev := map[string]any{
		"ts":     h.now().UTC().Format(time.RFC3339),
		"kind":   EventSourceUp,
		"device": cfg.DeviceCode,
	}
	if want != "" {
		ev["kind"] = EventSourceDown
		ev["reason"] = want
	}
	h.logger.Info("source state changed", "kind", ev["kind"], "reason", want)
	h.publish(h.eventTopic, ev)
}

// toReadings flattens a snapshot into unit-suffixed keys. Values the cloud
// did not report are omitted.
func toReadings(d *aq.DeviceData, st *aq.DeviceStatus, devName string, now time.Time) map[string]any {
	r := map[string]any{
		"ts":                now.UTC().Format(time.RFC3339),
		"device_code":       d.DeviceCode,
		"power":             d.Power,
		"mode":              string(d.Mode),
		"state":             string(d.State),
		"target_temp_c":     d.TargetTemp,
		"inlet_temp_c":      d.InletTemp,
		"outlet_temp_c":     d.OutletTemp,
		"ambient_temp_c":    d.AmbientTemp,
		"coil_temp_c":       d.CoilTemp,
		"water_temp_status": string(d.WaterTempStatus),
		"water_temps_valid": d.WaterTempsValid(),
		"online":            st.Online,
		"fault":             st.Fault,
	}
	if devName != "" {
		r["device_name"] = devName
	}
	if d.FanSpeed != "" {
		r["fan_speed"] = string(d.FanSpeed)
	}
	if v := d.ValidCurrentTemp(); v != nil {
		r["pool_temp_c"] = *v
	}
	optF := map[string]*float64{
		"target_min_c":           d.TargetMin,
		"target_max_c":           d.TargetMax,
		"suction_temp_c":         d.SuctionTemp,
		"exhaust_temp_c":         d.ExhaustTemp,
		"compressor_freq_hz":     d.CompressorFrequency,
		"compressor_max_freq_hz": d.CompressorMaxFrequency,
		"compressor_speed_pct":   d.CompressorSpeed,
		"compressor_current_a":   d.CompressorCurrent,
	}
	for k, v := range optF {
		if v != nil {
			r[k] = *v
		}
	}
	optB := map[string]*bool{
		"compressor_running": d.CompressorRunning,
		"water_flowing":      d.WaterFlowing,
		"external_stop":      d.ExternalStop,
	}
	for k, v := range optB {
		if v != nil {
			r[k] = *v
		}
	}
	if len(d.SensorFaults) > 0 {
		r["sensor_faults"] = d.SensorFaults
	}
	if len(st.Faults) > 0 {
		faults := make([]string, len(st.Faults))
		for i, f := range st.Faults {
			faults[i] = f.Description
		}
		r["faults"] = faults
	}
	return r
}

// publish marshals v to JSON and publishes it. Errors are logged only.
func (h *HeatPump) publish(topic string, v map[string]any) {
	if h.natsConn == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		h.logger.Error("marshal for NATS", "error", err)
		return
	}
	if err := h.natsConn.Publish(topic, b); err != nil {
		h.logger.Error("publish to NATS", "topic", topic, "error", err)
	}
}

// Name returns the unique resource identifier.
func (h *HeatPump) Name() resource.Name { return h.name }

// Readings returns the newest snapshot plus "data_age_s".
func (h *HeatPump) Readings(ctx context.Context) (map[string]any, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.readings == nil {
		if h.pollErr != nil {
			return nil, fmt.Errorf("no readings available, last poll error: %w", h.pollErr)
		}
		return nil, errors.New("no readings available yet")
	}
	out := make(map[string]any, len(h.readings)+1)
	for k, v := range h.readings {
		out[k] = v
	}
	out["data_age_s"] = int64(h.now().Sub(h.lastPollAt).Seconds())
	return out, nil
}

// Reconfigure applies new configuration and restarts polling.
func (h *HeatPump) Reconfigure(ctx context.Context, deps resource.Dependencies, conf resource.Config) error {
	cfg, err := parseConfig(registry.Config(conf.Attributes))
	if err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	client, err := clientFactory(cfg)
	if err != nil {
		return fmt.Errorf("creating aquatemp client: %w", err)
	}
	devName, err := resolveDevice(ctx, client, cfg)
	if err != nil {
		return err
	}

	if h.cancel != nil {
		h.cancel()
	}
	h.wg.Wait()

	h.pollMu.Lock()
	h.mu.Lock()
	if cfg.DeviceCode != h.config.DeviceCode {
		h.readings = nil
		h.downReason = ""
		h.lastState = ""
	}
	h.config, h.client, h.deviceName = cfg, client, devName
	h.pollErr = nil
	h.errCount = 0
	h.mu.Unlock()
	h.pollMu.Unlock()

	h.startPolling(ctx)
	return nil
}

// DoCommand handles ad-hoc commands:
//
//	{"command":"refresh"}
//	{"command":"get_config"}
//	{"command":"get_state"}
//	{"command":"set_power","on":true}
//	{"command":"set_mode","mode":"heat"}            // heat, cool, auto
//	{"command":"set_temperature","temp_c":28}
//	{"command":"set_fan_speed","speed":"low"}       // low, auto
//
// Set commands are validated by goaquatemp and followed by a refresh.
func (h *HeatPump) DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error) {
	h.mu.RLock()
	cfg, client := h.config, h.client
	h.mu.RUnlock()

	name, _ := cmd["command"].(string)
	var err error
	switch name {
	case "refresh":
		return h.refresh(ctx), nil
	case "get_config":
		return map[string]any{
			"device_code":   cfg.DeviceCode,
			"api_version":   cfg.APIVersion,
			"poll_interval": cfg.PollInterval.String(),
		}, nil
	case "get_state":
		h.pollMu.Lock()
		reason, errs := h.downReason, h.errCount
		h.pollMu.Unlock()
		st := map[string]any{"up": reason == "", "consecutive_errors": errs}
		if reason != "" {
			st["reason"] = reason
		}
		return st, nil
	case "set_power":
		on, ok := cmd["on"].(bool)
		if !ok {
			return nil, errors.New(`set_power requires boolean "on"`)
		}
		err = client.SetPower(ctx, cfg.DeviceCode, on)
	case "set_mode":
		mode, _ := cmd["mode"].(string)
		err = client.SetMode(ctx, cfg.DeviceCode, aq.OperationMode(mode))
	case "set_temperature":
		t, ok := cmd["temp_c"].(float64)
		if !ok {
			return nil, errors.New(`set_temperature requires number "temp_c"`)
		}
		err = client.SetTemperature(ctx, cfg.DeviceCode, t)
	case "set_fan_speed":
		speed, _ := cmd["speed"].(string)
		err = client.SetFanSpeed(ctx, cfg.DeviceCode, aq.FanSpeed(speed))
	default:
		return nil, fmt.Errorf("unknown command: %q", name)
	}
	if err != nil {
		return nil, err
	}
	return h.refresh(ctx), nil
}

func (h *HeatPump) refresh(ctx context.Context) map[string]any {
	h.poll(ctx)
	h.mu.RLock()
	err := h.pollErr
	h.mu.RUnlock()
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error()}
	}
	return map[string]any{"status": "ok"}
}

// Close stops polling and logs out.
func (h *HeatPump) Close(ctx context.Context) error {
	if h.cancel != nil {
		h.cancel()
	}
	h.wg.Wait()
	h.mu.RLock()
	client := h.client
	h.mu.RUnlock()
	return client.Logout(ctx)
}

var _ resource.Sensor = (*HeatPump)(nil)
