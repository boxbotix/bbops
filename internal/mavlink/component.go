package mavlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/c360studio/semengine/component"
	"github.com/c360studio/semengine/types"
)

const udpReadBufferBytes = 2048

// Component is the SemEngine `input` LifecycleComponent ("mavlink-udp") that binds
// a local MAVLink v2 UDP socket and routes `0xB1` vehicle telemetry across the
// 3-lane hybrid SKG architecture (`D1`–`D4` Option A).
type Component struct {
	settings  resolvedSettings
	deps      component.Dependencies
	logger    *slog.Logger
	tracker   *Tracker
	publisher *Publisher
	lifecycle component.Lifecycle

	conn      atomic.Pointer[net.UDPConn]
	boundAddr atomic.Pointer[netip.AddrPort]

	runMu     sync.Mutex
	runCancel context.CancelFunc
	wg        sync.WaitGroup
}

var (
	_ component.Discoverable       = (*Component)(nil)
	_ component.LifecycleComponent = (*Component)(nil)
)

// Register registers the "mavlink-udp" input component factory with registry.
func Register(registry *component.Registry) error {
	return RegisterWithHook(registry, nil)
}

// RegisterWithHook registers the "mavlink-udp" factory and invokes onCreated
// whenever a Component instance is constructed (used by host.Runtime to expose
// peer endpoint lookup without global state).
func RegisterWithHook(registry *component.Registry, onCreated func(*Component)) error {
	if registry == nil {
		return errors.New("mavlink: nil component registry")
	}
	return registry.Register(component.Registration{
		Name:        FactoryName,
		Type:        types.ComponentTypeInput,
		Protocol:    "mavlink",
		Domain:      "robotics",
		Description: "Clean-room 0xB1 MAVLink v2 UDP telemetry and 3-lane SKG ingest adapter",
		Version:     "0.1.0",
		Schema:      configSchema,
		Factory: func(rawConfig json.RawMessage, deps component.Dependencies) (component.Discoverable, error) {
			comp, err := NewComponent(rawConfig, deps)
			if err != nil {
				return nil, err
			}
			if onCreated != nil {
				onCreated(comp)
			}
			return comp, nil
		},
		Ports: declarePorts,
	})
}

// NewComponent validates rawConfig and constructs an unstarted MAVLink v2 UDP Component.
func NewComponent(rawConfig json.RawMessage, deps component.Dependencies) (*Component, error) {
	settings, err := resolveConfig(rawConfig)
	if err != nil {
		return nil, err
	}
	org := deps.Platform.Org
	if org == "" {
		org = "boxbotix"
	}
	platform := deps.Platform.Platform
	if platform == "" {
		platform = "bbops"
	}
	tracker, err := NewTracker(org, platform)
	if err != nil {
		return nil, err
	}
	logger := deps.GetLoggerWithComponent(DefaultInstanceName)
	c := &Component{
		settings: settings,
		deps:     deps,
		logger:   logger,
		tracker:  tracker,
	}
	c.publisher = NewPublisher(
		settings.entitySubject,
		func(ctx context.Context, subject string, msgs [][]byte) error {
			if c.deps.NATSClient == nil {
				return errors.New("mavlink: nil NATS client")
			}
			return c.deps.NATSClient.PublishBatchToStream(ctx, subject, msgs)
		},
		tracker.BuildUrgentBatch,
		c.lifecycle.CountError,
	)
	return c, nil
}

// InputPorts implements component.Discoverable.
func (c *Component) InputPorts() []component.Port {
	return []component.Port{c.settings.inputPort}
}

// OutputPorts implements component.Discoverable.
func (c *Component) OutputPorts() []component.Port {
	out := make([]component.Port, len(c.settings.outputPorts))
	copy(out, c.settings.outputPorts)
	return out
}

// Health implements component.Discoverable without blocking on Start or Stop.
func (c *Component) Health() component.HealthStatus {
	return c.lifecycle.Health()
}

// DataFlow implements component.Discoverable without blocking on Start or Stop.
func (c *Component) DataFlow() component.FlowMetrics {
	return c.lifecycle.DataFlow()
}

// Initialize implements component.LifecycleComponent.
func (c *Component) Initialize() error {
	return c.lifecycle.Initialize()
}

// Start binds the configured UDP listener and launches the UDP read loop,
// periodic SKG snapshot ticker, and non-blocking JetStream batch publisher (`ADR-113`).
func (c *Component) Start(ctx context.Context) error {
	return c.lifecycle.Start(ctx, c.start, c.cleanup)
}

// Stop cancels background workers, closes the UDP listener, and waits for all
// goroutines to exit within ctx's deadline (`ADR-108` / `ADR-113`).
func (c *Component) Stop(ctx context.Context) error {
	return c.lifecycle.Stop(ctx)
}

// BoundAddr returns the local UDP address bound by Start, or an invalid AddrPort before Start.
func (c *Component) BoundAddr() netip.AddrPort {
	if ptr := c.boundAddr.Load(); ptr != nil {
		return *ptr
	}
	return netip.AddrPort{}
}

// UDPConn returns the underlying bound *net.UDPConn while running, or nil before Start / after Stop.
func (c *Component) UDPConn() *net.UDPConn {
	return c.conn.Load()
}

// PeerAddr returns the latest validated peer UDP address recorded for sysid (`D4 Option A`).
func (c *Component) PeerAddr(sysid uint8) (netip.AddrPort, bool) {
	return c.tracker.PeerAddr(sysid)
}

// Peers returns a snapshot map of all admitted sysids and their latest UDP peer endpoints.
func (c *Component) Peers() map[uint8]netip.AddrPort {
	return c.tracker.Peers()
}

func (c *Component) start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.deps.NATSClient == nil {
		return errors.New("mavlink: NATSClient dependency is required")
	}

	addrStr := net.JoinHostPort(c.settings.udpHost, strconv.Itoa(c.settings.udpPort))
	udpAddr, err := net.ResolveUDPAddr("udp", addrStr)
	if err != nil {
		return fmt.Errorf("mavlink: resolve UDP address %q: %w", addrStr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("mavlink: listen UDP %q: %w", addrStr, err)
	}
	c.conn.Store(conn)
	if localUDP, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		ap := localUDP.AddrPort()
		c.boundAddr.Store(&ap)
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	c.runMu.Lock()
	c.runCancel = runCancel
	c.runMu.Unlock()

	c.wg.Add(3)
	go func() {
		defer c.wg.Done()
		c.readLoop(runCtx, conn)
	}()
	go func() {
		defer c.wg.Done()
		c.tickLoop(runCtx)
	}()
	go func() {
		defer c.wg.Done()
		c.publisher.Run(runCtx)
	}()

	return nil
}

func (c *Component) cleanup(ctx context.Context) error {
	c.runMu.Lock()
	cancel := c.runCancel
	c.runCancel = nil
	c.runMu.Unlock()
	if cancel != nil {
		cancel()
	}

	var closeErr error
	if conn := c.conn.Swap(nil); conn != nil {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErr = fmt.Errorf("mavlink: close UDP socket: %w", err)
		}
	}

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return closeErr
	case <-ctx.Done():
		return errors.Join(closeErr, ctx.Err())
	}
}

func (c *Component) readLoop(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, udpReadBufferBytes)
	for {
		n, peerAddr, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			c.lifecycle.CountError()
			continue
		}
		if n <= 0 {
			continue
		}

		now := time.Now().UTC()
		outcome := c.tracker.IngestDatagram(buf[:n], peerAddr, now)
		if outcome.CountedError {
			c.lifecycle.CountError()
			continue
		}
		if !outcome.Accepted {
			continue
		}
		c.lifecycle.CountInput(n)

		if outcome.Telemetry != nil {
			c.publishCoreJSON(ctx, c.telemetrySubject(outcome.SysID), outcome.Telemetry)
		}
		if outcome.Event != nil {
			c.publishCoreJSON(ctx, c.eventSubject(outcome.SysID), outcome.Event)
		}
		if outcome.UrgentSKGSync {
			c.publisher.NotifyUrgent(outcome.SysID)
		}
	}
}

func (c *Component) tickLoop(ctx context.Context) {
	ticker := time.NewTicker(c.settings.skgInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			batch, err := c.tracker.BuildPeriodicBatch()
			if err != nil {
				c.lifecycle.CountError()
				continue
			}
			c.publisher.EnqueuePeriodic(batch)
		}
	}
}

func (c *Component) publishCoreJSON(ctx context.Context, subject string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		c.lifecycle.CountError()
		return
	}
	if err := c.deps.NATSClient.Publish(ctx, subject, raw); err != nil && ctx.Err() == nil {
		c.lifecycle.CountError()
	}
}

func (c *Component) telemetrySubject(sysid uint8) string {
	return c.settings.telemetryPrefix + "." + strconv.FormatUint(uint64(sysid), 10)
}

func (c *Component) eventSubject(sysid uint8) string {
	return c.settings.telemetryPrefix + ".events." + strconv.FormatUint(uint64(sysid), 10)
}
