# gorai-aquatemp

Gorai satellite component: an Aqua Temp / Linked-Go heat pump as a `sensor/aquatemp`
component, built on the goaquatemp SDK (`github.com/emergingrobotics/goaquatemp`, a normal go.mod
dependency; for local SDK work, gorai-all's go.work uses `./goaquatemp`). Same pattern as
`gorai-pentair` (`../gorai-pentair` in the gorai-all workspace). See README.md for subjects, payload and commands.

## Layout

- `heatpump/aquatemp.go`: component (registry `sensor`/`aquatemp`), poll loop, NATS publish, events, DoCommand
- `heatpump/aquatemp_test.go`: fake `goaquatemp.Client` + embedded nats-server
- `cmd/gorai-aquatemp`: standalone publisher to a NATS server (mesh registration optional)
- `cmd/aquatemp-watch`: subscriber that prints `gorai.*.heat-pump.>`

## Rules

- Omit values the cloud did not report; never publish them as 0.
- Never publish water temps as the pool temperature without flow. `pool_temp_c` comes from `ValidCurrentTemp()`.
- Device writes only go through goaquatemp's validated setters.
- Don't poll the cloud faster than 30s (it throttles aggressive clients).
- Build/test: `make check`, `make test-race`. Host bus: `make nats-up`, `make run`, `make watch`.
