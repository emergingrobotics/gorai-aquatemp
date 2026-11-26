# gorai-aquatemp

A [gorai](https://github.com/emergingrobotics/gorai) satellite component that puts an
Aqua Temp / Linked-Go pool heat pump on the gorai NATS mesh. It wraps
[goaquatemp](https://github.com/emergingrobotics/goaquatemp), polls the Aqua Temp
cloud, and publishes the heat pump's state as a standard `sensor/aquatemp` component.

## Quick start: publish to a NATS bus on this host

```bash
make nats-up                       # NATS 2.12 + JetStream in Docker on :4222 (monitor :8222)

export AQUATEMP_USERNAME=you@example.com
export AQUATEMP_PASSWORD=...
make run                           # polls every 2m, publishes to gorai.home.heat-pump.*

make watch                         # in another shell: pretty-print what is published
make nats-down                     # when finished
```

Point at a different server with `NATS_URL=nats://host:4222 make run`, or run the
binary directly:

```bash
./bin/gorai-aquatemp --nats nats://localhost:4222 --robot-id home --name heat-pump --poll-interval 1m
```

| Flag | Default | Description |
|---|---|---|
| `--nats` | `$NATS_URL` or `nats://localhost:4222` | NATS server URL |
| `--robot-id` | `robot` | First segment of the subjects |
| `--name` | `heat-pump` | Component name |
| `--username` / `--password` | `$AQUATEMP_USERNAME` / `$AQUATEMP_PASSWORD` | Aqua Temp account |
| `--device` | `$AQUATEMP_DEFAULT_DEVICE`, else auto-detect | Device code (required if the account has several) |
| `--api-version` | `$AQUATEMP_API_VERSION`, else `modern` | `legacy`, `hitemp` or `modern` |
| `--poll-interval` | `2m` | Minimum `30s` |
| `--no-mesh` | off | Skip mesh registration (NATS without JetStream) |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error` |

## Embedded in a robot binary

```go
import _ "github.com/emergingrobotics/gorai-aquatemp/heatpump"
```

```json
{
  "name": "heat-pump",
  "type": "sensor",
  "model": "aquatemp",
  "attributes": {
    "username": "${AQUATEMP_USERNAME}",
    "password": "${AQUATEMP_PASSWORD}",
    "poll_interval": "2m"
  }
}
```

Attributes: `username`, `password` (required), `device_code`, `api_version`,
`base_url`, `poll_interval`, `robot_id`, `name`. The component publishes only when the
runtime gives it a `nats` dependency; `Readings()` works either way.

## Subjects

### `gorai.<robot_id>.<name>.data`: one message per successful poll

| Key | Unit | Notes |
|---|---|---|
| `ts` | RFC 3339 | Poll time (UTC) |
| `device_code`, `device_name` | | |
| `power`, `mode`, `state`, `fan_speed` | | `state` is what it is doing: `off`, `idle`, `heating`, `cooling`, `defrosting` |
| `target_temp_c`, `target_min_c`, `target_max_c` | °C | |
| `inlet_temp_c`, `outlet_temp_c` | °C | Raw. Stagnant pipe water when there is no flow |
| `pool_temp_c` | °C | Inlet temperature, **only when it can be trusted** (omitted on no flow) |
| `water_flowing`, `water_temp_status`, `water_temps_valid` | | `valid`, `no_flow`, `unknown` |
| `ambient_temp_c`, `coil_temp_c`, `suction_temp_c`, `exhaust_temp_c` | °C | |
| `compressor_running`, `compressor_freq_hz`, `compressor_max_freq_hz`, `compressor_speed_pct`, `compressor_current_a` | | |
| `external_stop` | | A wired input is holding the unit off |
| `online`, `fault`, `faults`, `sensor_faults` | | |

Fields the cloud does not report are left out, never sent as `0`. `Readings()`
returns the same map plus `data_age_s`.

### `gorai.<robot_id>.<name>.event`: state changes

```json
{"ts":"...","kind":"source_down","reason":"api_error","device":"289C..."}
{"ts":"...","kind":"source_down","reason":"device_offline","device":"289C..."}
{"ts":"...","kind":"source_up","device":"289C..."}
{"ts":"...","kind":"state_change","device":"289C...","from":"heating","to":"defrosting"}
```

`api_error` fires after 3 consecutive failed polls.

## DoCommand

| Command | Args |
|---|---|
| `refresh` | |
| `get_config` | |
| `get_state` | |
| `set_power` | `on` (bool) |
| `set_mode` | `mode`: `heat`, `cool`, `auto` |
| `set_temperature` | `temp_c`: 15–40 |
| `set_fan_speed` | `speed`: `auto`, `low` |

goaquatemp validates the set commands. Each one is followed by a refresh. They are
not exposed on NATS by the standalone binary.

## Local development

```
er/
  goaquatemp/              # github.com/user/goaquatemp (replace in go.mod)
  gorai-all/
    go.work                # includes ./gorai-aquatemp
    gorai/
    gorai-aquatemp/        # this repo
```

```bash
make test-race   # unit tests: fake Aqua Temp client + in-process NATS server
make check
```
