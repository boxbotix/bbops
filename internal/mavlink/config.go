package mavlink

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/c360studio/semengine/component"
)

const (
	// FactoryName is the SemEngine component factory name registered for MAVLink v2 UDP ingest.
	FactoryName = "mavlink-udp"
	// DefaultInstanceName is the canonical instance name composed in configs/bbops.json.
	DefaultInstanceName = "mavlink-ingest"

	// DefaultUDPHost is the default local edge bind address (127.0.0.1).
	DefaultUDPHost = "127.0.0.1"
	// DefaultUDPPort is the standard local MAVLink edge UDP port (14550).
	DefaultUDPPort = 14550

	// DefaultSKGHz is the default Lane 2 coalesced SKG snapshot publish rate (1 Hz).
	DefaultSKGHz = 1.0
	// MinSKGHz is the minimum supported Lane 2 SKG snapshot rate (1 Hz).
	MinSKGHz = 1.0
	// MaxSKGHz is the maximum supported Lane 2 SKG snapshot rate (5 Hz).
	MaxSKGHz = 5.0

	// DefaultTelemetrySubject is the core NATS output wildcard covering Lane 1 and Lane 3.
	DefaultTelemetrySubject = "bbops.telemetry.>"
	// DefaultTelemetryPrefix is the concrete subject prefix derived from DefaultTelemetrySubject.
	DefaultTelemetryPrefix = "bbops.telemetry"
	// DefaultEntitySubject is the JetStream ENTITY subject for Lane 2 and Lane 3 SKG snapshots.
	DefaultEntitySubject = "entity.uxs.upsert"

	minUserPort = 1024
	maxUserPort = 65535
)

// Config describes the JSON configuration accepted by the "mavlink-udp" SemEngine input factory.
type Config struct {
	SKGHz float64              `json:"skg_hz,omitempty" schema:"type:float,description:Coalesced SKG snapshot publish rate in Hz (1..5),default:1,min:1,max:5"`
	Ports component.PortConfig `json:"ports" schema:"type:ports,description:External MAVLink UDP input and NATS/JetStream outputs"`
}

var configSchema = component.GenerateConfigSchema(reflect.TypeOf(Config{}))

type resolvedSettings struct {
	udpHost         string
	udpPort         int
	skgHz           float64
	skgInterval     time.Duration
	telemetryPrefix string
	entitySubject   string
	inputPort       component.Port
	outputPorts     []component.Port
}

func resolveConfig(rawConfig json.RawMessage) (resolvedSettings, error) {
	var wire struct {
		SKGHz *float64              `json:"skg_hz"`
		Ports *component.PortConfig `json:"ports"`
	}
	trimmed := bytes.TrimSpace(rawConfig)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&wire); err != nil {
			return resolvedSettings{}, fmt.Errorf("mavlink: decode config: %w", err)
		}
	}

	skgHz := DefaultSKGHz
	if wire.SKGHz != nil {
		skgHz = *wire.SKGHz
	}
	if math.IsNaN(skgHz) || math.IsInf(skgHz, 0) || skgHz < MinSKGHz || skgHz > MaxSKGHz {
		return resolvedSettings{}, fmt.Errorf("mavlink: skg_hz %v out of range [%v, %v]", skgHz, MinSKGHz, MaxSKGHz)
	}

	var portCfg component.PortConfig
	if wire.Ports != nil {
		portCfg = *wire.Ports
	}
	if len(portCfg.Inputs) == 0 {
		portCfg.Inputs = []component.PortDefinition{
			{
				Name:        "mavlink_udp",
				Required:    true,
				External:    true,
				Description: "Inbound 0xB1 MAVLink v2 UDP datagrams",
				Config: component.NetworkPort{
					Protocol: "udp",
					Host:     DefaultUDPHost,
					Port:     DefaultUDPPort,
				},
			},
		}
	}
	if len(portCfg.Outputs) == 0 {
		portCfg.Outputs = []component.PortDefinition{
			{
				Name:        "telemetry",
				Description: "Fast 20 Hz kinematics and instant edge transition events",
				Config: component.NATSPort{
					Subject: DefaultTelemetrySubject,
				},
			},
			{
				Name:        "entity_upsert",
				Description: "Coalesced 1-5 Hz and edge-burst VehicleSnapshot SKG upserts",
				Config: component.NATSPort{
					Subject: DefaultEntitySubject,
				},
			},
		}
	}

	if len(portCfg.Inputs) != 1 {
		return resolvedSettings{}, fmt.Errorf("mavlink: expected 1 UDP input port, got %d", len(portCfg.Inputs))
	}
	inPort, err := portCfg.Inputs[0].Resolve(component.DirectionInput)
	if err != nil {
		return resolvedSettings{}, fmt.Errorf("mavlink: resolve input port: %w", err)
	}
	netCfg, ok := inPort.Config.(component.NetworkPort)
	if !ok || !strings.EqualFold(netCfg.Protocol, "udp") {
		return resolvedSettings{}, errors.New("mavlink: input port must be a network port with protocol \"udp\"")
	}
	if netCfg.Port < minUserPort || netCfg.Port > maxUserPort {
		return resolvedSettings{}, fmt.Errorf("mavlink: UDP port %d out of range %d..%d", netCfg.Port, minUserPort, maxUserPort)
	}

	if len(portCfg.Outputs) != 2 {
		return resolvedSettings{}, fmt.Errorf("mavlink: expected 2 NATS output ports (telemetry, entity_upsert), got %d", len(portCfg.Outputs))
	}
	outputs := make([]component.Port, 0, len(portCfg.Outputs))
	var telemetrySubject, entitySubject string
	for _, def := range portCfg.Outputs {
		outPort, resolveErr := def.Resolve(component.DirectionOutput)
		if resolveErr != nil {
			return resolvedSettings{}, fmt.Errorf("mavlink: resolve output port %q: %w", def.Name, resolveErr)
		}
		natsCfg, isNATS := outPort.Config.(component.NATSPort)
		if !isNATS || strings.TrimSpace(natsCfg.Subject) == "" {
			return resolvedSettings{}, fmt.Errorf("mavlink: output port %q must be a non-empty nats port", def.Name)
		}
		switch def.Name {
		case "telemetry":
			if telemetrySubject != "" {
				return resolvedSettings{}, errors.New("mavlink: duplicate \"telemetry\" output port")
			}
			telemetrySubject = natsCfg.Subject
		case "entity_upsert":
			if entitySubject != "" {
				return resolvedSettings{}, errors.New("mavlink: duplicate \"entity_upsert\" output port")
			}
			entitySubject = natsCfg.Subject
		default:
			return resolvedSettings{}, fmt.Errorf("mavlink: unknown output port %q (expected \"telemetry\" or \"entity_upsert\")", def.Name)
		}
		outputs = append(outputs, outPort)
	}

	telemetryPrefix, err := deriveTelemetryPrefix(telemetrySubject)
	if err != nil {
		return resolvedSettings{}, err
	}
	if strings.ContainsAny(entitySubject, "*> \t\r\n") || strings.HasPrefix(entitySubject, ".") || strings.HasSuffix(entitySubject, ".") {
		return resolvedSettings{}, fmt.Errorf("mavlink: entity_upsert subject %q must be a concrete NATS subject without wildcards", entitySubject)
	}

	intervalNs := math.Round(float64(time.Second) / skgHz)
	return resolvedSettings{
		udpHost:         netCfg.Host,
		udpPort:         netCfg.Port,
		skgHz:           skgHz,
		skgInterval:     time.Duration(intervalNs),
		telemetryPrefix: telemetryPrefix,
		entitySubject:   entitySubject,
		inputPort:       inPort,
		outputPorts:     outputs,
	}, nil
}

func deriveTelemetryPrefix(subject string) (string, error) {
	trimmed := strings.TrimSpace(subject)
	for _, suffix := range []string{".>", ".*"} {
		if strings.HasSuffix(trimmed, suffix) {
			trimmed = strings.TrimSuffix(trimmed, suffix)
			break
		}
	}
	if trimmed == "" || strings.ContainsAny(trimmed, "*> \t\r\n") || strings.HasPrefix(trimmed, ".") || strings.HasSuffix(trimmed, ".") {
		return "", fmt.Errorf("mavlink: invalid telemetry subject %q", subject)
	}
	return trimmed, nil
}

func declarePorts(rawConfig json.RawMessage, _ string) (component.PortConfig, error) {
	s, err := resolveConfig(rawConfig)
	if err != nil {
		return component.PortConfig{}, err
	}
	return component.PortConfigFrom([]component.Port{s.inputPort}, s.outputPorts), nil
}
