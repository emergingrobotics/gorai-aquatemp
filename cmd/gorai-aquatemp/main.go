// Command gorai-aquatemp runs the Aqua Temp heat pump component standalone:
// it connects to a NATS server (by default the one on this host), polls the
// Aqua Temp cloud and publishes readings and events to the bus.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/emergingrobotics/gorai/pkg/mesh"
	"github.com/emergingrobotics/gorai/pkg/registry"
	"github.com/nats-io/nats.go"

	"github.com/emergingrobotics/gorai-aquatemp/heatpump"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	natsURL := flag.String("nats", envOr("NATS_URL", nats.DefaultURL), "NATS server URL (or NATS_URL)")
	robotID := flag.String("robot-id", "robot", "Robot ID: first segment of gorai.<robot-id>.<name>.data")
	name := flag.String("name", "heat-pump", "Component name")
	username := flag.String("username", "", "Aqua Temp username (or AQUATEMP_USERNAME)")
	password := flag.String("password", "", "Aqua Temp password (or AQUATEMP_PASSWORD)")
	device := flag.String("device", "", "Device code; auto-detected for single-device accounts (or AQUATEMP_DEFAULT_DEVICE)")
	apiVersion := flag.String("api-version", "", "API version: legacy, hitemp, modern (or AQUATEMP_API_VERSION)")
	pollInterval := flag.String("poll-interval", "2m", "Poll interval (minimum 30s)")
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	noMesh := flag.Bool("no-mesh", false, "Skip mesh service discovery (use when NATS has no JetStream)")
	showVersion := flag.Bool("version", false, "Show version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("gorai-aquatemp %s (commit %s, built %s)\n", version, commit, buildTime)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))
	slog.SetDefault(logger)

	conf := registry.Config{
		"name":          *name,
		"robot_id":      *robotID,
		"username":      orEnv(*username, "AQUATEMP_USERNAME"),
		"password":      orEnv(*password, "AQUATEMP_PASSWORD"),
		"device_code":   orEnv(*device, "AQUATEMP_DEFAULT_DEVICE"),
		"api_version":   orEnv(*apiVersion, "AQUATEMP_API_VERSION"),
		"poll_interval": *pollInterval,
	}
	if conf["username"] == "" || conf["password"] == "" {
		fmt.Fprintln(os.Stderr, "error: username and password required (flags or AQUATEMP_USERNAME / AQUATEMP_PASSWORD)")
		os.Exit(1)
	}

	nc, err := nats.Connect(*natsURL,
		nats.Name("gorai-aquatemp"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) { logger.Warn("NATS disconnected", "error", err) }),
		nats.ReconnectHandler(func(c *nats.Conn) { logger.Info("NATS reconnected", "url", c.ConnectedUrl()) }),
	)
	if err != nil {
		logger.Error("connect to NATS", "url", *natsURL, "error", err)
		os.Exit(1)
	}
	defer nc.Close()
	logger.Info("connected to NATS", "url", nc.ConnectedUrl())

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	comp, err := heatpump.New(ctx, natsDeps{nc}, conf)
	if err != nil {
		logger.Error("create component", "error", err)
		os.Exit(1)
	}
	hp := comp.(*heatpump.HeatPump)

	var reg *mesh.Registration
	if !*noMesh {
		if mc, err := mesh.NewClient(nc, mesh.WithClientLogger(logger)); err != nil {
			logger.Warn("mesh discovery unavailable, continuing without it (--no-mesh to silence)", "error", err)
		} else if reg, err = mc.Register(ctx, mesh.ServiceDescriptor{
			Name:     *name,
			Type:     mesh.TypeComponent,
			Subtype:  "sensor",
			Model:    "aquatemp",
			RobotID:  *robotID,
			Metadata: map[string]string{"version": version},
		}); err != nil {
			logger.Warn("mesh registration failed, continuing without it", "error", err)
			reg = nil
		} else {
			logger.Info("registered with mesh service discovery")
		}
	}

	logger.Info("publishing",
		"data", fmt.Sprintf("gorai.%s.%s.data", *robotID, *name),
		"events", fmt.Sprintf("gorai.%s.%s.event", *robotID, *name),
		"interval", *pollInterval)

	<-ctx.Done()
	logger.Info("shutting down")
	if reg != nil {
		reg.Deregister()
	}
	closeCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	if err := hp.Close(closeCtx); err != nil {
		logger.Warn("close component", "error", err)
	}
	if err := nc.Drain(); err != nil {
		logger.Warn("drain NATS", "error", err)
	}
}

// natsDeps hands the NATS connection to the component, as the gorai runtime
// does in a robot binary.
type natsDeps struct{ nc *nats.Conn }

func (d natsDeps) Get(name string) (any, error) {
	if name == "nats" {
		return d.nc, nil
	}
	return nil, fmt.Errorf("dependency %q not available", name)
}

func (d natsDeps) GetByType(subtype string) ([]any, error) {
	return nil, fmt.Errorf("no dependencies of type %q", subtype)
}

func orEnv(v, key string) string {
	if v != "" {
		return v
	}
	return os.Getenv(key)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}
