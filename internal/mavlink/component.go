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

	"github.com/boxbotix/bbops/internal/mavcodec"
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

	// sysidMu serializes per-vehicle Tracker transitions with Core NATS Lane 1 / Lane 3
	// publication across readLoop and tickLoop while keeping JetStream acks off both paths.
	sysidMu [256]sync.Mutex

	actuationHooks atomic.Pointer[actuationCallbacks]
	wallNowFn      atomic.Pointer[func() time.Time]

	// Test-only synchronization hooks (nil in production).
	beforeIngestFrame func(frame mavcodec.Frame)
	afterSilenceEval  func(ev TransitionEvent)

	runMu     sync.Mutex
	runCancel context.CancelFunc
	wg        sync.WaitGroup
}

type actuationCallbacks struct {
	onEvent    func(TransitionEvent)
	onAck      func(sysid uint8, ack mavcodec.CommandAck, observedAt time.Time)
	onAckFrame func(sysid, seq uint8, ack mavcodec.CommandAck, observedAt time.Time)
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
	tracker, err := NewTrackerWithOptions(org, platform, settings.linkTimeout, settings.supervisoryTimeout, settings.allowedPeers)
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
		tracker.BuildPeriodicBatch,
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

// PeerAddr returns the pinned validated peer UDP address recorded for sysid (`OQ1 Option A`).
func (c *Component) PeerAddr(sysid uint8) (netip.AddrPort, bool) {
	return c.tracker.PeerAddr(sysid)
}

// Peers returns a snapshot map of all admitted sysids and their pinned UDP peer endpoints.
func (c *Component) Peers() map[uint8]netip.AddrPort {
	return c.tracker.Peers()
}

// ActuationTarget returns the pinned UDP endpoint and current safety state for sysid.
func (c *Component) ActuationTarget(sysid uint8) (ActuationTarget, bool) {
	return c.tracker.ActuationTarget(sysid)
}

// ResolveObservation resolves an inbound operator observation against the underlying Tracker.
func (c *Component) ResolveObservation(sysid uint8, obsRev, authorityEpoch uint64, observedMs int64) (uint64, bool) {
	return c.tracker.ResolveObservation(sysid, obsRev, authorityEpoch, observedMs)
}

// SetWallClock configures an optional wall-clock source for UTC timestamps while
// keeping monotonic elapsed-time silence tracking anchored to time.Now().
func (c *Component) SetWallClock(fn func() time.Time) {
	if fn == nil {
		c.wallNowFn.Store(nil)
		return
	}
	c.wallNowFn.Store(&fn)
}

func (c *Component) currentClocks() (time.Time, time.Time) {
	mono := time.Now()
	if ptr := c.wallNowFn.Load(); ptr != nil && *ptr != nil {
		return (*ptr)(), mono
	}
	return mono, mono
}

// SetActuationCallbacks registers synchronous callbacks invoked on Lane 3 state
// transitions (`heartbeat_edge`, `link_lost`, etc.) and `#77 COMMAND_ACK` ingest.
func (c *Component) SetActuationCallbacks(
	onEvent func(TransitionEvent),
	onAck func(sysid uint8, ack mavcodec.CommandAck, observedAt time.Time),
) {
	if onEvent == nil && onAck == nil {
		c.actuationHooks.Store(nil)
		return
	}
	c.actuationHooks.Store(&actuationCallbacks{
		onEvent: onEvent,
		onAck:   onAck,
	})
}

// SetActuationFrameCallbacks registers synchronous callbacks invoked on Lane 3 state
// transitions and `#77 COMMAND_ACK` ingest with the originating MAVLink v2 frame sequence.
func (c *Component) SetActuationFrameCallbacks(
	onEvent func(TransitionEvent),
	onAckFrame func(sysid, seq uint8, ack mavcodec.CommandAck, observedAt time.Time),
) {
	if onEvent == nil && onAckFrame == nil {
		c.actuationHooks.Store(nil)
		return
	}
	c.actuationHooks.Store(&actuationCallbacks{
		onEvent:    onEvent,
		onAckFrame: onAckFrame,
	})
}

// SendFrame encodes frame as a MAVLink v2 (`0xFD`) packet and transmits it over
// the bound UDP socket to peer.
func (c *Component) SendFrame(frame mavcodec.Frame, peer netip.AddrPort) error {
	if !peer.IsValid() {
		return errors.New("mavlink: invalid peer address")
	}
	conn := c.UDPConn()
	if conn == nil {
		return errors.New("mavlink: UDP socket is not running")
	}
	raw, err := mavcodec.EncodeFrame(frame)
	if err != nil {
		return err
	}
	_, err = conn.WriteToUDPAddrPort(raw, peer)
	return err
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

		frame, decErr := mavcodec.DecodeFrame(buf[:n])
		if decErr != nil {
			c.lifecycle.CountError()
			continue
		}
		if c.beforeIngestFrame != nil {
			c.beforeIngestFrame(frame)
		}
		c.ingestDecodedFrame(ctx, frame, peerAddr, n)
	}
}

func (c *Component) ingestDecodedFrame(ctx context.Context, frame mavcodec.Frame, peerAddr netip.AddrPort, n int) {
	wallNow, monoNow := c.currentClocks()
	mu := &c.sysidMu[frame.SystemID]
	mu.Lock()
	outcome := c.tracker.IngestFrameWithClock(frame, peerAddr, wallNow, monoNow)
	if outcome.CountedError {
		c.lifecycle.CountError()
		mu.Unlock()
		return
	}
	if !outcome.Accepted {
		mu.Unlock()
		return
	}
	c.lifecycle.CountInput(n)

	for i := range outcome.Events {
		c.publishCoreJSON(ctx, c.eventSubject(outcome.SysID), outcome.Events[i])
	}
	if outcome.Telemetry != nil {
		c.publishCoreJSON(ctx, c.telemetrySubject(outcome.SysID), outcome.Telemetry)
	}
	if outcome.UrgentSKGSync {
		c.publisher.NotifyUrgent(outcome.SysID)
	}
	mu.Unlock()

	if hooks := c.actuationHooks.Load(); hooks != nil {
		if hooks.onEvent != nil {
			for i := range outcome.Events {
				hooks.onEvent(outcome.Events[i])
			}
		}
		if outcome.CommandAck != nil {
			if hooks.onAckFrame != nil {
				hooks.onAckFrame(outcome.SysID, outcome.CommandAckSeq, *outcome.CommandAck, wallNow)
			} else if hooks.onAck != nil {
				hooks.onAck(outcome.SysID, *outcome.CommandAck, wallNow)
			}
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
			c.evaluateSilenceTick(ctx)
		}
	}
}

func (c *Component) evaluateSilenceTick(ctx context.Context) {
	hooks := c.actuationHooks.Load()
	for _, sysid := range c.tracker.admittedSysIDs() {
		wallNow, monoNow := c.currentClocks()
		mu := &c.sysidMu[sysid]
		mu.Lock()
		ev, lost := c.tracker.evaluateSilenceSysIDAt(sysid, wallNow, monoNow)
		if lost {
			if c.afterSilenceEval != nil {
				c.afterSilenceEval(ev)
			}
			c.publishCoreJSON(ctx, c.eventSubject(ev.SysID), ev)
		}
		mu.Unlock()
		if lost && hooks != nil && hooks.onEvent != nil {
			hooks.onEvent(ev)
		}
	}
	c.publisher.NotifyPeriodic()
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
