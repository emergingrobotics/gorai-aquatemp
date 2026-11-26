package heatpump

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/emergingrobotics/gorai/pkg/registry"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	aq "github.com/emergingrobotics/goaquatemp"
)

// fakeClient is an in-memory aq.Client.
type fakeClient struct {
	mu       sync.Mutex
	devices  []aq.Device
	data     aq.DeviceData
	status   aq.DeviceStatus
	err      error
	controls []aq.ControlCommand
	loggedIn bool
}

func (f *fakeClient) Login(context.Context) error {
	f.mu.Lock()
	f.loggedIn = true
	f.mu.Unlock()
	return nil
}
func (f *fakeClient) Logout(context.Context) error {
	f.mu.Lock()
	f.loggedIn = false
	f.mu.Unlock()
	return nil
}
func (f *fakeClient) GetDevices(context.Context) ([]aq.Device, error) {
	return f.devices, nil
}
func (f *fakeClient) GetDevice(context.Context, string) (*aq.Device, error) { return nil, nil }
func (f *fakeClient) GetDeviceData(_ context.Context, code string) (*aq.DeviceData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	d := f.data
	d.DeviceCode = code
	return &d, nil
}
func (f *fakeClient) GetDeviceStatus(context.Context, string) (*aq.DeviceStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.status
	return &s, nil
}
func (f *fakeClient) control(c aq.ControlCommand) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.controls = append(f.controls, c)
	return nil
}
func (f *fakeClient) SetTemperature(_ context.Context, code string, t float64) error {
	if err := aq.ValidateTemperature(t); err != nil {
		return err
	}
	return f.control(aq.ControlCommand{DeviceCode: code, TargetTemp: &t})
}
func (f *fakeClient) SetMode(_ context.Context, code string, m aq.OperationMode) error {
	return f.control(aq.ControlCommand{DeviceCode: code, Mode: &m})
}
func (f *fakeClient) SetPower(_ context.Context, code string, on bool) error {
	return f.control(aq.ControlCommand{DeviceCode: code, Power: &on})
}
func (f *fakeClient) SetFanSpeed(_ context.Context, code string, s aq.FanSpeed) error {
	return f.control(aq.ControlCommand{DeviceCode: code, FanSpeed: &s})
}
func (f *fakeClient) Control(_ context.Context, c aq.ControlCommand) error { return f.control(c) }
func (f *fakeClient) GetHistory(context.Context, string, aq.HistoryQuery) (*aq.History, error) {
	return nil, nil
}
func (f *fakeClient) GetParameters(context.Context, string, aq.ParameterKind) ([]aq.Parameter, error) {
	return nil, nil
}
func (f *fakeClient) IsAuthenticated() bool { return f.loggedIn }
func (f *fakeClient) Config() *aq.Config    { return aq.DefaultConfig() }

func (f *fakeClient) set(fn func(*fakeClient)) { f.mu.Lock(); fn(f); f.mu.Unlock() }

func ptr[T any](v T) *T { return &v }

func newFake() *fakeClient {
	return &fakeClient{
		devices: []aq.Device{{Code: "DEV1", Name: "Pool"}},
		data: aq.DeviceData{
			Power: true, Mode: aq.ModeHeat, State: aq.StateHeating, FanSpeed: aq.FanAuto,
			TargetTemp: 28, InletTemp: 26.5, OutletTemp: 27.8, AmbientTemp: 18, CoilTemp: 10,
			CompressorRunning: ptr(true), CompressorFrequency: ptr(55.0),
			WaterFlowing: ptr(true), WaterTempStatus: aq.WaterTempValid, CurrentTemp: 26.5,
		},
		status: aq.DeviceStatus{Online: true, Status: "ONLINE"},
	}
}

func baseConf() registry.Config {
	return registry.Config{"username": "u", "password": "p", "name": "heat-pump", "robot_id": "home"}
}

func startNATS(t *testing.T) *nats.Conn {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func subscribe(t *testing.T, nc *nats.Conn, subj string) chan map[string]any {
	t.Helper()
	ch := make(chan map[string]any, 16)
	if _, err := nc.Subscribe(subj, func(m *nats.Msg) {
		var v map[string]any
		if err := json.Unmarshal(m.Data, &v); err != nil {
			t.Errorf("bad json on %s: %v", m.Subject, err)
		}
		ch <- v
	}); err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return ch
}

func recv(t *testing.T, ch chan map[string]any) map[string]any {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
		return nil
	}
}

func noMsg(t *testing.T, ch chan map[string]any) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("unexpected message: %v", v)
	case <-time.After(100 * time.Millisecond):
	}
}

// newTest builds a component against the fake without starting the loop.
func newTest(t *testing.T, fc *fakeClient, nc *nats.Conn) *HeatPump {
	t.Helper()
	cfg, err := parseConfig(baseConf())
	if err != nil {
		t.Fatal(err)
	}
	devName, err := resolveDevice(context.Background(), fc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return newHeatPump(baseConf(), cfg, fc, devName, nc, time.Now)
}

func TestParseConfig(t *testing.T) {
	cases := []struct {
		name    string
		mod     func(registry.Config)
		wantErr bool
	}{
		{"ok", func(registry.Config) {}, false},
		{"no username", func(c registry.Config) { delete(c, "username") }, true},
		{"no password", func(c registry.Config) { delete(c, "password") }, true},
		{"bad interval", func(c registry.Config) { c["poll_interval"] = "soon" }, true},
		{"short interval", func(c registry.Config) { c["poll_interval"] = "5s" }, true},
		{"bad api version", func(c registry.Config) { c["api_version"] = "v9" }, true},
		{"modern", func(c registry.Config) { c["api_version"] = "modern" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConf()
			tc.mod(c)
			_, err := parseConfig(c)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestResolveDevice(t *testing.T) {
	fc := newFake()
	cfg := &Config{}
	if _, err := resolveDevice(context.Background(), fc, cfg); err != nil || cfg.DeviceCode != "DEV1" {
		t.Fatalf("auto-detect: code=%q err=%v", cfg.DeviceCode, err)
	}

	fc.devices = append(fc.devices, aq.Device{Code: "DEV2"})
	if _, err := resolveDevice(context.Background(), fc, &Config{}); err == nil {
		t.Fatal("want error for multiple devices without device_code")
	}
	if _, err := resolveDevice(context.Background(), fc, &Config{DeviceCode: "NOPE"}); err == nil {
		t.Fatal("want error for unknown device_code")
	}
	cfg = &Config{DeviceCode: "DEV2"}
	if _, err := resolveDevice(context.Background(), fc, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestPollPublishesReadings(t *testing.T) {
	nc := startNATS(t)
	data := subscribe(t, nc, "gorai.home.heat-pump.data")
	fc := newFake()
	h := newTest(t, fc, nc)

	h.poll(context.Background())
	r := recv(t, data)

	for k, want := range map[string]any{
		"device_code": "DEV1", "device_name": "Pool", "state": "heating", "mode": "heat",
		"power": true, "target_temp_c": 28.0, "pool_temp_c": 26.5, "compressor_freq_hz": 55.0,
		"water_temp_status": "valid", "online": true,
	} {
		if r[k] != want {
			t.Errorf("%s = %v, want %v", k, r[k], want)
		}
	}
	for _, k := range []string{"suction_temp_c", "compressor_speed_pct", "faults", "external_stop"} {
		if _, ok := r[k]; ok {
			t.Errorf("%s present but not reported by device", k)
		}
	}

	got, err := h.Readings(context.Background())
	if err != nil || got["state"] != "heating" {
		t.Fatalf("Readings = %v, %v", got, err)
	}
}

func TestNoFlowOmitsPoolTemp(t *testing.T) {
	fc := newFake()
	fc.data.WaterFlowing = ptr(false)
	fc.data.WaterTempStatus = aq.WaterTempNoFlow
	h := newTest(t, fc, nil)
	h.poll(context.Background())
	r, _ := h.Readings(context.Background())
	if _, ok := r["pool_temp_c"]; ok {
		t.Fatal("pool_temp_c must be omitted with no flow")
	}
	if r["inlet_temp_c"] != 26.5 || r["water_temps_valid"] != false {
		t.Fatalf("raw inlet / validity wrong: %v", r)
	}
}

func TestEvents(t *testing.T) {
	nc := startNATS(t)
	ev := subscribe(t, nc, "gorai.home.heat-pump.event")
	fc := newFake()
	h := newTest(t, fc, nc)
	ctx := context.Background()

	h.poll(ctx) // first poll: up, no event
	noMsg(t, ev)

	fc.set(func(f *fakeClient) { f.data.State = aq.StateDefrosting })
	h.poll(ctx)
	if e := recv(t, ev); e["kind"] != EventStateChange || e["from"] != "heating" || e["to"] != "defrosting" {
		t.Fatalf("state change event = %v", e)
	}

	fc.set(func(f *fakeClient) { f.status.Online = false })
	h.poll(ctx)
	if e := recv(t, ev); e["kind"] != EventSourceDown || e["reason"] != ReasonDeviceOffline {
		t.Fatalf("offline event = %v", e)
	}

	fc.set(func(f *fakeClient) { f.status.Online = true })
	h.poll(ctx)
	if e := recv(t, ev); e["kind"] != EventSourceUp {
		t.Fatalf("up event = %v", e)
	}

	fc.set(func(f *fakeClient) { f.err = errors.New("boom") })
	h.poll(ctx)
	h.poll(ctx)
	noMsg(t, ev) // below threshold
	h.poll(ctx)
	if e := recv(t, ev); e["kind"] != EventSourceDown || e["reason"] != ReasonAPIError {
		t.Fatalf("api error event = %v", e)
	}

	fc.set(func(f *fakeClient) { f.err = nil })
	h.poll(ctx)
	if e := recv(t, ev); e["kind"] != EventSourceUp {
		t.Fatalf("recovery event = %v", e)
	}
}

func TestDoCommand(t *testing.T) {
	fc := newFake()
	h := newTest(t, fc, nil)
	ctx := context.Background()

	if _, err := h.DoCommand(ctx, map[string]any{"command": "set_temperature", "temp_c": 29.0}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DoCommand(ctx, map[string]any{"command": "set_temperature", "temp_c": 90.0}); err == nil {
		t.Fatal("want validation error for 90°C")
	}
	if _, err := h.DoCommand(ctx, map[string]any{"command": "set_power", "on": false}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DoCommand(ctx, map[string]any{"command": "set_power"}); err == nil {
		t.Fatal("want error for missing on")
	}
	if len(fc.controls) != 2 || *fc.controls[0].TargetTemp != 29 || *fc.controls[1].Power {
		t.Fatalf("controls = %+v", fc.controls)
	}
	if st, _ := h.DoCommand(ctx, map[string]any{"command": "get_state"}); st["up"] != true {
		t.Fatalf("get_state = %v", st)
	}
	if c, _ := h.DoCommand(ctx, map[string]any{"command": "get_config"}); c["device_code"] != "DEV1" {
		t.Fatalf("get_config = %v", c)
	}
	if _, err := h.DoCommand(ctx, map[string]any{"command": "explode"}); err == nil {
		t.Fatal("want error for unknown command")
	}
}

func TestNewViaRegistryAndClose(t *testing.T) {
	nc := startNATS(t)
	data := subscribe(t, nc, "gorai.home.heat-pump.data")
	fc := newFake()
	old := clientFactory
	clientFactory = func(*Config) (aq.Client, error) { return fc, nil }
	t.Cleanup(func() { clientFactory = old })

	comp, err := New(context.Background(), deps{nc}, baseConf())
	if err != nil {
		t.Fatal(err)
	}
	recv(t, data) // initial poll published
	if err := comp.(*HeatPump).Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fc.IsAuthenticated() {
		t.Fatal("Close should log out")
	}
}

type deps struct{ nc *nats.Conn }

func (d deps) Get(string) (any, error)         { return d.nc, nil }
func (d deps) GetByType(string) ([]any, error) { return nil, nil }
