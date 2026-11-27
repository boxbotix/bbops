package e2eharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/boxbotix/bbops/internal/host"
	"github.com/boxbotix/bbops/internal/mavcodec"
	"github.com/boxbotix/bbops/internal/uxsvocab"
	"github.com/c360studio/semengine/graph"
	"github.com/c360studio/semengine/message"
	"github.com/c360studio/semengine/payloadregistry"
	semtypes "github.com/c360studio/semengine/pkg/types"
	"github.com/c360studio/semengine/vocabulary"
)

const (
	wgs84SemiMajorM     = 6378137.0
	wgs84Flattening     = 1.0 / 298.257223563
	wgs84EccentricitySq = wgs84Flattening * (2.0 - wgs84Flattening)

	defaultOriginLatDeg = 37.7749
	defaultOriginLonDeg = -122.4194
	defaultOriginAltM   = 10.0

	defaultTelemetryInterval = 50 * time.Millisecond
)

var (
	externalContactPayloadType = message.Type{Domain: "cop", Category: "contact", Version: "v1"}
	missionNodePayloadType     = message.Type{Domain: "ops", Category: "mission", Version: "v1"}
)

// Options configures an ephemeral loopback bbops E2E stack.
type Options struct {
	UIDir             string
	DataDir           string
	TelemetryInterval time.Duration
}

// VehicleIDs identifies the 3 canonical multi-domain UXS entities streamed by Stack.
type VehicleIDs struct {
	Quadcopter  string `json:"quadcopter"`
	Rover       string `json:"rover"`
	SurfaceBoat string `json:"surfaceBoat"`
}

// StackInfo summarizes the bound endpoints and seeded entity IDs for a running Stack.
type StackInfo struct {
	Ready             bool       `json:"ready"`
	BaseURL           string     `json:"baseURL"`
	UDPAddr           string     `json:"udpAddr"`
	Org               string     `json:"org"`
	Platform          string     `json:"platform"`
	Vehicles          VehicleIDs `json:"vehicles"`
	ExternalContactID string     `json:"externalContactId"`
	CorridorEntityID  string     `json:"corridorEntityId"`
}

// ExternalContactSeed is a non-UXS external COP contact Graphable fixture published
// into `entity.>` -> `graph-ingest` -> `ENTITY_STATES` for E2E verification.
type ExternalContactSeed struct {
	ID                string     `json:"id"`
	Title             string     `json:"title"`
	ContactType       string     `json:"contact_type"`
	Source            string     `json:"source,omitempty"`
	FeedStatus        string     `json:"feed_status,omitempty"`
	Latitude          float64    `json:"latitude"`
	Longitude         float64    `json:"longitude"`
	Altitude          *float64   `json:"altitude,omitempty"`
	ObservedAt        *time.Time `json:"observed_at,omitempty"`
	ReceivedAt        *time.Time `json:"received_at,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	UncertaintyMeters *float64   `json:"uncertainty_meters,omitempty"`
	MonitorsEntityID  string     `json:"monitors_entity_id,omitempty"`
	CorridorEntityID  string     `json:"corridor_entity_id,omitempty"`
}

func (s *ExternalContactSeed) Schema() message.Type { return externalContactPayloadType }

func (s *ExternalContactSeed) IndexingProfile() string { return vocabulary.IndexingProfileControl }

func (s *ExternalContactSeed) Validate() error {
	if s == nil {
		return errors.New("e2eharness: nil external contact seed")
	}
	parts := strings.Split(s.ID, ".")
	if len(parts) != 6 || parts[3] != "cop" {
		return fmt.Errorf("e2eharness: external contact ID %q must have 6 segments with domain 'cop'", s.ID)
	}
	if _, err := semtypes.ParseEntityID(s.ID); err != nil {
		return fmt.Errorf("e2eharness: invalid external contact ID %q: %w", s.ID, err)
	}
	if strings.TrimSpace(s.Title) == "" {
		return errors.New("e2eharness: external contact title required")
	}
	if math.IsNaN(s.Latitude) || math.IsInf(s.Latitude, 0) || s.Latitude < -90 || s.Latitude > 90 {
		return fmt.Errorf("e2eharness: invalid latitude %v", s.Latitude)
	}
	if math.IsNaN(s.Longitude) || math.IsInf(s.Longitude, 0) || s.Longitude < -180 || s.Longitude > 180 {
		return fmt.Errorf("e2eharness: invalid longitude %v", s.Longitude)
	}
	if s.MonitorsEntityID != "" {
		if _, err := semtypes.ParseEntityID(s.MonitorsEntityID); err != nil {
			return fmt.Errorf("e2eharness: invalid monitors_entity_id %q: %w", s.MonitorsEntityID, err)
		}
	}
	if s.CorridorEntityID != "" {
		if _, err := semtypes.ParseEntityID(s.CorridorEntityID); err != nil {
			return fmt.Errorf("e2eharness: invalid corridor_entity_id %q: %w", s.CorridorEntityID, err)
		}
	}
	for _, tr := range s.Triples() {
		if _, err := vocabulary.ParsePredicate(tr.Predicate); err != nil {
			return fmt.Errorf("e2eharness: invalid predicate %q: %w", tr.Predicate, err)
		}
	}
	return nil
}

func (s *ExternalContactSeed) MarshalJSON() ([]byte, error) {
	type alias ExternalContactSeed
	return json.Marshal((*alias)(s))
}

func (s *ExternalContactSeed) UnmarshalJSON(data []byte) error {
	type alias ExternalContactSeed
	return json.Unmarshal(data, (*alias)(s))
}

func (s *ExternalContactSeed) EntityID() string { return s.ID }

func (s *ExternalContactSeed) Triples() []message.Triple {
	if s == nil || s.ID == "" {
		return nil
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	src := s.Source
	if src == "" {
		src = "e2e-cop-fixture"
	}
	add := func(pred string, obj any) message.Triple {
		return message.Triple{
			Subject:    s.ID,
			Predicate:  pred,
			Object:     obj,
			Source:     src,
			Timestamp:  now,
			Confidence: 1.0,
		}
	}
	triples := []message.Triple{
		add("dc.terms.title", s.Title),
		add("geo.location.latitude", s.Latitude),
		add("geo.location.longitude", s.Longitude),
	}
	if s.Altitude != nil {
		triples = append(triples, add("geo.location.altitude", *s.Altitude))
	}
	if s.ContactType != "" {
		triples = append(triples, add("cop.contact.type", s.ContactType))
	}
	if s.Source != "" {
		triples = append(triples, add("cop.contact.source", s.Source))
	}
	if s.FeedStatus != "" {
		triples = append(triples, add("cop.feed.status", s.FeedStatus))
	}
	if s.ObservedAt != nil {
		triples = append(triples, add("cop.time.observed", s.ObservedAt.UTC().Format(time.RFC3339Nano)))
	}
	if s.ReceivedAt != nil {
		triples = append(triples, add("cop.time.received", s.ReceivedAt.UTC().Format(time.RFC3339Nano)))
	}
	if s.ExpiresAt != nil {
		triples = append(triples, add("cop.time.expires", s.ExpiresAt.UTC().Format(time.RFC3339Nano)))
	}
	if s.UncertaintyMeters != nil {
		triples = append(triples, add("cop.location.uncertainty-m", *s.UncertaintyMeters))
	}
	if s.MonitorsEntityID != "" {
		triples = append(triples, add("cop.contact.monitors", s.MonitorsEntityID))
	}
	if s.CorridorEntityID != "" {
		triples = append(triples, add("cop.mission.corridor", s.CorridorEntityID))
	}
	return triples
}

type missionNodeSeed struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func (m *missionNodeSeed) Schema() message.Type    { return missionNodePayloadType }
func (m *missionNodeSeed) IndexingProfile() string { return vocabulary.IndexingProfileControl }
func (m *missionNodeSeed) EntityID() string        { return m.ID }
func (m *missionNodeSeed) MarshalJSON() ([]byte, error) {
	type alias missionNodeSeed
	return json.Marshal((*alias)(m))
}
func (m *missionNodeSeed) UnmarshalJSON(data []byte) error {
	type alias missionNodeSeed
	return json.Unmarshal(data, (*alias)(m))
}
func (m *missionNodeSeed) Validate() error {
	if m == nil || m.ID == "" {
		return errors.New("e2eharness: mission node id required")
	}
	_, err := semtypes.ParseEntityID(m.ID)
	return err
}
func (m *missionNodeSeed) Triples() []message.Triple {
	return []message.Triple{
		{
			Subject:    m.ID,
			Predicate:  "dc.terms.title",
			Object:     m.Title,
			Source:     "e2e-mission-fixture",
			Timestamp:  time.Now().UTC().Truncate(time.Millisecond),
			Confidence: 1.0,
		},
	}
}

var (
	_ graph.Graphable = (*ExternalContactSeed)(nil)
	_ graph.Graphable = (*missionNodeSeed)(nil)
)

// LocalNEDToLLA converts a local tangent-plane NED offset [northM, eastM, downM]
// from originLLA [lat0Deg, lon0Deg, alt0M] into WGS-84 [latDeg, lonDeg, altM].
func LocalNEDToLLA(ned [3]float64, originLLA [3]float64) ([3]float64, error) {
	lat0Deg, lon0Deg, alt0M := originLLA[0], originLLA[1], originLLA[2]
	if math.IsNaN(lat0Deg) || math.IsInf(lat0Deg, 0) || lat0Deg < -90 || lat0Deg > 90 ||
		math.IsNaN(lon0Deg) || math.IsInf(lon0Deg, 0) || lon0Deg < -180 || lon0Deg > 180 ||
		math.IsNaN(alt0M) || math.IsInf(alt0M, 0) {
		return [3]float64{}, fmt.Errorf("e2eharness: invalid origin LLA %v", originLLA)
	}
	for _, v := range ned {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return [3]float64{}, fmt.Errorf("e2eharness: non-finite NED %v", ned)
		}
	}
	lat0Rad := lat0Deg * math.Pi / 180.0
	sinLat := math.Sin(lat0Rad)
	denomSq := 1.0 - wgs84EccentricitySq*sinLat*sinLat
	denom := math.Sqrt(denomSq)
	primeVerticalM := wgs84SemiMajorM / denom
	meridionalM := (wgs84SemiMajorM * (1.0 - wgs84EccentricitySq)) / (denomSq * denom)
	cosLat0 := math.Max(math.Cos(lat0Rad), 1e-9)

	dLatDeg := (ned[0] / (meridionalM + alt0M)) * (180.0 / math.Pi)
	dLonDeg := (ned[1] / ((primeVerticalM + alt0M) * cosLat0)) * (180.0 / math.Pi)
	latDeg := math.Max(-90.0, math.Min(90.0, lat0Deg+dLatDeg))
	lonRad := math.Remainder((lon0Deg+dLonDeg)*(math.Pi/180.0), 2.0*math.Pi)
	lonDeg := lonRad * (180.0 / math.Pi)
	altM := alt0M - ned[2]
	return [3]float64{latDeg, lonDeg, altM}, nil
}

// EulerZYXDegToQuaternion converts ZYX (3-2-1, NED-to-FRD) Euler angles in degrees
// into a normalized Hamilton quaternion [w, x, y, z].
func EulerZYXDegToQuaternion(rollDeg, pitchDeg, yawDeg float64) [4]float32 {
	r := (rollDeg * math.Pi / 180.0) * 0.5
	p := (pitchDeg * math.Pi / 180.0) * 0.5
	y := (yawDeg * math.Pi / 180.0) * 0.5
	cr, sr := math.Cos(r), math.Sin(r)
	cp, sp := math.Cos(p), math.Sin(p)
	cy, sy := math.Cos(y), math.Sin(y)
	w := cr*cp*cy + sr*sp*sy
	x := sr*cp*cy - cr*sp*sy
	yQ := cr*sp*cy + sr*cp*sy
	z := cr*cp*sy - sr*sp*cy
	norm := math.Sqrt(w*w + x*x + yQ*yQ + z*z)
	if norm <= 1e-9 {
		return [4]float32{1, 0, 0, 0}
	}
	return [4]float32{
		float32(w / norm),
		float32(x / norm),
		float32(yQ / norm),
		float32(z / norm),
	}
}

type vehicleSimState struct {
	sysid                  uint8
	domain                 mavcodec.VehicleDomain
	armed                  bool
	authority              mavcodec.ControlAuthority
	mode                   mavcodec.CompanionMode
	linkTier               mavcodec.LinkTier
	flags                  uxsvocab.HealthFlags
	ned                    [3]float64
	vel                    [3]float64
	cmdVel                 [3]float64
	crossCurrentNED        [2]float64
	hasInjectedCurrent     bool
	yawRateDegS            float64
	teleopControlled       bool
	teleopActive           bool
	lastTeleopAt           time.Time
	blackoutUntil          time.Time
	suppressHeartbeatUntil time.Time
	failsafeCount          uint16
	dropAckCount           int
	gcsHeartbeatCount      int
	rollDeg                float64
	pitchDeg               float64
	yawDeg                 float64
	seq                    uint8
	conn                   *net.UDPConn
}

// Stack manages a live ephemeral loopback bbops runtime and deterministic 0xB1
// multi-domain telemetry peers for headless browser E2E suites.
type Stack struct {
	rt          *host.Runtime
	ownedTmpDir string
	serverUDP   *net.UDPAddr
	info        StackInfo

	mu       sync.Mutex
	vehicles map[uint8]*vehicleSimState
	rxWg     sync.WaitGroup
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// StartStack boots an ephemeral loopback bbops runtime serving opts.UIDir, streams
// 20 Hz 0xB1 telemetry for Quadcopter (#1), Rover (#2), and crabbing SurfaceBoat (#3),
// and seeds external COP contact + 2-hop mission fixtures into ENTITY_STATES.
func StartStack(ctx context.Context, opts Options) (*Stack, error) {
	uiDir := strings.TrimSpace(opts.UIDir)
	if uiDir == "" {
		return nil, errors.New("e2eharness: UIDir is required")
	}
	indexInfo, err := os.Stat(filepath.Join(uiDir, "index.html"))
	if err != nil || indexInfo.IsDir() {
		return nil, fmt.Errorf("e2eharness: UIDir %q missing compiled index.html", uiDir)
	}

	var ownedTmpDir string
	dataDir := strings.TrimSpace(opts.DataDir)
	if dataDir == "" {
		ownedTmpDir, err = os.MkdirTemp("", "bbops-e2e-data-*")
		if err != nil {
			return nil, fmt.Errorf("e2eharness: create temp data dir: %w", err)
		}
		dataDir = ownedTmpDir
	}

	interval := opts.TelemetryInterval
	if interval <= 0 {
		interval = defaultTelemetryInterval
	}

	cleanupTmp := func() {
		if ownedTmpDir != "" {
			_ = os.RemoveAll(ownedTmpDir)
		}
	}

	rt, err := host.StartEphemeral(ctx, host.Options{
		DataDir: dataDir,
		UIDir:   uiDir,
		Version: "v0.1.0-e2e",
		Overrides: host.PortOverrides{
			MetricsPort: -1,
		},
		RegisterPayloads: func(reg *payloadregistry.Registry) error {
			if regErr := reg.Register(&payloadregistry.Registration{
				Factory:     func() any { return &ExternalContactSeed{} },
				Domain:      externalContactPayloadType.Domain,
				Category:    externalContactPayloadType.Category,
				Version:     externalContactPayloadType.Version,
				Description: "E2E external COP contact fixture payload",
			}); regErr != nil {
				return regErr
			}
			return reg.Register(&payloadregistry.Registration{
				Factory:     func() any { return &missionNodeSeed{} },
				Domain:      missionNodePayloadType.Domain,
				Category:    missionNodePayloadType.Category,
				Version:     missionNodePayloadType.Version,
				Description: "E2E mission corridor neighbor fixture payload",
			})
		},
	}, 4)
	if err != nil {
		cleanupTmp()
		return nil, fmt.Errorf("e2eharness: start runtime: %w", err)
	}

	readyCtx, readyCancel := context.WithTimeout(ctx, 20*time.Second)
	err = rt.WaitReady(readyCtx)
	readyCancel()
	if err != nil {
		_ = rt.Stop(context.Background())
		cleanupTmp()
		return nil, fmt.Errorf("e2eharness: wait ready: %w", err)
	}

	endpoints := rt.Endpoints()
	serverUDP, err := net.ResolveUDPAddr("udp", endpoints.MAVLinkUDPAddr)
	if err != nil {
		_ = rt.Stop(context.Background())
		cleanupTmp()
		return nil, fmt.Errorf("e2eharness: resolve UDP %q: %w", endpoints.MAVLinkUDPAddr, err)
	}

	platform := rt.Platform()
	quadID, err := uxsvocab.VehicleEntityID(platform.Org, platform.Platform, 1)
	if err != nil {
		_ = rt.Stop(context.Background())
		cleanupTmp()
		return nil, err
	}
	roverID, err := uxsvocab.VehicleEntityID(platform.Org, platform.Platform, 2)
	if err != nil {
		_ = rt.Stop(context.Background())
		cleanupTmp()
		return nil, err
	}
	boatID, err := uxsvocab.VehicleEntityID(platform.Org, platform.Platform, 3)
	if err != nil {
		_ = rt.Stop(context.Background())
		cleanupTmp()
		return nil, err
	}

	contactID := fmt.Sprintf("%s.%s.sensor.cop.vessel.harbor-buoy-7", platform.Org, platform.Platform)
	corridorID := fmt.Sprintf("%s.%s.mission.ops.corridor.channel-north", platform.Org, platform.Platform)

	stack := &Stack{
		rt:          rt,
		ownedTmpDir: ownedTmpDir,
		serverUDP:   serverUDP,
		info: StackInfo{
			Ready:             true,
			BaseURL:           endpoints.HTTPBaseURL,
			UDPAddr:           endpoints.MAVLinkUDPAddr,
			Org:               platform.Org,
			Platform:          platform.Platform,
			Vehicles:          VehicleIDs{Quadcopter: quadID, Rover: roverID, SurfaceBoat: boatID},
			ExternalContactID: contactID,
			CorridorEntityID:  corridorID,
		},
		vehicles: make(map[uint8]*vehicleSimState, 3),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}

	healthyFlags := uxsvocab.HealthFlags{
		ClockSyncLocked:       true,
		TeleopDeadmanClamped:  false,
		CRSFLinkHealthy:       true,
		CompanionTrunkHealthy: true,
	}
	specs := []*vehicleSimState{
		{
			sysid:     1,
			domain:    mavcodec.DomainQuadcopter,
			armed:     true,
			authority: mavcodec.AuthorityCompanionTeleop,
			mode:      mavcodec.ModeTeleopVelocity,
			linkTier:  mavcodec.LinkTierSITLLocal,
			flags:     healthyFlags,
			ned:       [3]float64{24.0, -14.0, -128.5},
			vel:       [3]float64{11.2, 3.6, -1.8},
			rollDeg:   14.0,
			pitchDeg:  -8.0,
			yawDeg:    132.0,
		},
		{
			sysid:     2,
			domain:    mavcodec.DomainRover,
			armed:     true,
			authority: mavcodec.AuthorityCompanionAutonomous,
			mode:      mavcodec.ModeAutonomousVelocity,
			linkTier:  mavcodec.LinkTierMeshHighRate,
			flags:     healthyFlags,
			ned:       [3]float64{-18.0, 28.0, 0.0},
			vel:       [3]float64{2.4, 1.2, 0.0},
			rollDeg:   0.0,
			pitchDeg:  2.0,
			yawDeg:    63.0,
		},
		{
			sysid:     3,
			domain:    mavcodec.DomainSurfaceBoat,
			armed:     true,
			authority: mavcodec.AuthorityCompanionTeleop,
			mode:      mavcodec.ModeTeleopVelocity,
			linkTier:  mavcodec.LinkTierDirectBench,
			flags:     healthyFlags,
			// Bow points due East (90 deg) while cross-current pushes North (+2.2 m/s North, +3.8 m/s East => ~-30 deg crab).
			ned:      [3]float64{8.0, 32.0, 0.0},
			vel:      [3]float64{2.2, 3.8, 0.0},
			rollDeg:  3.0,
			pitchDeg: 1.0,
			yawDeg:   90.0,
		},
	}

	for _, vs := range specs {
		conn, listenErr := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		if listenErr != nil {
			_ = stack.Stop(context.Background())
			return nil, fmt.Errorf("e2eharness: open vehicle %d UDP socket: %w", vs.sysid, listenErr)
		}
		vs.conn = conn
		stack.vehicles[vs.sysid] = vs
	}
	for _, vs := range specs {
		stack.rxWg.Add(1)
		go stack.readVehicleLoop(vs.sysid, vs.conn)
	}

	stack.mu.Lock()
	burstErr := stack.sendAllTelemetryLocked(true)
	stack.mu.Unlock()
	if burstErr != nil {
		_ = stack.Stop(context.Background())
		return nil, burstErr
	}

	nowUTC := time.Now().UTC().Truncate(time.Millisecond)
	observedUTC := nowUTC.Add(-12500 * time.Millisecond)
	contactLLA, err := LocalNEDToLLA([3]float64{-26.0, -24.0, 0.0}, [3]float64{defaultOriginLatDeg, defaultOriginLonDeg, defaultOriginAltM})
	if err != nil {
		_ = stack.Stop(context.Background())
		return nil, err
	}
	if err := stack.publishGraphable(ctx, &missionNodeSeed{
		ID:    corridorID,
		Title: "North Harbor Transit Corridor",
	}); err != nil {
		_ = stack.Stop(context.Background())
		return nil, err
	}
	// Seed distinct ObservedAt vs ReceivedAt while omitting ExpiresAt and UncertaintyMeters so both delayed-feed freshness and unknown-preserving fields are verified.
	if err := stack.UpsertExternalContact(ctx, ExternalContactSeed{
		ID:               contactID,
		Title:            "AIS Vessel Harbor-7",
		ContactType:      "surface-vessel",
		Source:           "ais-receiver-1",
		FeedStatus:       "live",
		Latitude:         contactLLA[0],
		Longitude:        contactLLA[1],
		ObservedAt:       &observedUTC,
		ReceivedAt:       &nowUTC,
		MonitorsEntityID: quadID,
		CorridorEntityID: corridorID,
	}); err != nil {
		_ = stack.Stop(context.Background())
		return nil, err
	}

	go stack.runLoop(interval)

	if err := stack.waitInitialConvergence(ctx); err != nil {
		_ = stack.Stop(context.Background())
		return nil, err
	}

	return stack, nil
}

// Info returns the resolved HTTP/UDP endpoints and canonical seeded entity IDs.
func (s *Stack) Info() StackInfo { return s.info }

// UpsertExternalContact validates and publishes a non-UXS external COP contact
// entity into `entity.>` -> `graph-ingest` -> `ENTITY_STATES` and awaits KV convergence.
func (s *Stack) UpsertExternalContact(ctx context.Context, seed ExternalContactSeed) error {
	if err := seed.Validate(); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	kv, err := s.rt.NATSClient().GetKeyValueBucket(waitCtx, graph.BucketEntityStates)
	if err != nil {
		return fmt.Errorf("e2eharness: lookup ENTITY_STATES: %w", err)
	}
	var prevRev uint64
	if prior, getErr := kv.Get(waitCtx, seed.ID); getErr == nil && prior != nil {
		prevRev = prior.Revision()
	}
	if err := s.publishGraphable(waitCtx, &seed); err != nil {
		return err
	}
	for {
		if entry, getErr := kv.Get(waitCtx, seed.ID); getErr == nil && entry != nil && entry.Revision() > prevRev {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("e2eharness: timed out awaiting ENTITY_STATES upsert for %q: %w", seed.ID, waitCtx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (s *Stack) publishGraphable(ctx context.Context, payload message.Payload) error {
	graphable, ok := payload.(graph.Graphable)
	if !ok {
		return errors.New("e2eharness: payload does not implement graph.Graphable")
	}
	wireBytes, err := json.Marshal(message.NewBaseMessage(payload.Schema(), payload, "bbops-e2e-harness"))
	if err != nil {
		return fmt.Errorf("e2eharness: marshal BaseMessage: %w", err)
	}
	return s.rt.NATSClient().PublishToStream(ctx, "entity."+graphable.EntityID(), wireBytes)
}

// PulseCRSFOverride switches sysid to P1 DirectCRSFOverride (HoldPosition + TeleopDeadmanClamped)
// and immediately sends a 0xB1 HEARTBEAT frame so Lane 3 emits an authority_change TransitionEvent.
func (s *Stack) PulseCRSFOverride(sysid uint8) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs, ok := s.vehicles[sysid]
	if !ok {
		return fmt.Errorf("e2eharness: unknown vehicle sysid %d", sysid)
	}
	vs.authority = mavcodec.AuthorityDirectCRSFOverride
	vs.mode = mavcodec.ModeHoldPosition
	vs.linkTier = mavcodec.LinkTierMeshHighRate
	vs.flags.TeleopDeadmanClamped = true
	vs.teleopActive = false
	vs.cmdVel = [3]float64{}
	vs.yawRateDegS = 0
	if vs.teleopControlled {
		recomputeEffectiveVelLocked(vs)
	}
	return s.sendVehicleHeartbeatLocked(vs)
}

// RestoreCompanionTeleop returns sysid to armed P2 CompanionTeleop (ModeTeleopVelocity)
// while preserving its active LinkTier and immediately transmits a 0xB1 HEARTBEAT frame.
func (s *Stack) RestoreCompanionTeleop(sysid uint8) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs, ok := s.vehicles[sysid]
	if !ok {
		return fmt.Errorf("e2eharness: unknown vehicle sysid %d", sysid)
	}
	vs.armed = true
	vs.authority = mavcodec.AuthorityCompanionTeleop
	vs.mode = mavcodec.ModeTeleopVelocity
	vs.blackoutUntil = time.Time{}
	vs.flags.CompanionTrunkHealthy = true
	vs.flags.TeleopDeadmanClamped = false
	return s.sendVehicleHeartbeatLocked(vs)
}

// SetVehicleLinkTier switches sysid's 0xB1 LinkTier (`sitl-local`, `direct-bench`,
// `mesh-high-rate`, or `mesh-supervisory`) and immediately emits a 0xB1 HEARTBEAT frame.
func (s *Stack) SetVehicleLinkTier(sysid uint8, tier string) error {
	var parsed mavcodec.LinkTier
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case uxsvocab.LinkTierSITLLocal:
		parsed = mavcodec.LinkTierSITLLocal
	case uxsvocab.LinkTierDirectBench:
		parsed = mavcodec.LinkTierDirectBench
	case uxsvocab.LinkTierMeshHighRate:
		parsed = mavcodec.LinkTierMeshHighRate
	case uxsvocab.LinkTierMeshSupervisory:
		parsed = mavcodec.LinkTierMeshSupervisory
	default:
		return fmt.Errorf("e2eharness: unsupported link tier %q", tier)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	vs, ok := s.vehicles[sysid]
	if !ok {
		return fmt.Errorf("e2eharness: unknown vehicle sysid %d", sysid)
	}
	vs.linkTier = parsed
	if parsed == mavcodec.LinkTierMeshSupervisory && vs.teleopActive {
		vs.teleopActive = false
		vs.cmdVel = [3]float64{}
		vs.yawRateDegS = 0
		recomputeEffectiveVelLocked(vs)
	}
	return s.sendVehicleHeartbeatLocked(vs)
}

// SetDropCommandAck configures sysid to drop the next count outbound #77 COMMAND_ACK
// replies (while still applying the command and delaying the next #0 HEARTBEAT past
// 1.5s so ADR-111 commit_unknown and subsequent heartbeat reconciliation are exercised).
func (s *Stack) SetDropCommandAck(sysid uint8, count int) error {
	if count < 0 {
		return errors.New("e2eharness: drop count cannot be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	vs, ok := s.vehicles[sysid]
	if !ok {
		return fmt.Errorf("e2eharness: unknown vehicle sysid %d", sysid)
	}
	vs.dropAckCount = count
	return nil
}

// GCSHeartbeatCount returns the number of outbound MAV_TYPE_GCS (#0) heartbeats
// received on sysid's simulated UDP peer socket from bbops.
func (s *Stack) GCSHeartbeatCount(sysid uint8) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs, ok := s.vehicles[sysid]
	if !ok {
		return 0
	}
	return vs.gcsHeartbeatCount
}

// EmitStatusText transmits a MAVLink #253 STATUSTEXT datagram from sysid's UDP peer socket.
func (s *Stack) EmitStatusText(sysid uint8, severity uint8, text string) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || len(trimmed) > mavcodec.MaxStatusTextBytes {
		return fmt.Errorf("e2eharness: statustext %q must be 1..%d bytes", text, mavcodec.MaxStatusTextBytes)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	vs, ok := s.vehicles[sysid]
	if !ok {
		return fmt.Errorf("e2eharness: unknown vehicle sysid %d", sysid)
	}
	return s.sendFrameLocked(vs, mavcodec.StatusText{
		Severity: severity,
		Text:     trimmed,
	})
}

// Stop halts background telemetry streaming, closes peer UDP sockets, stops the
// embedded runtime, and removes any temporary JetStream data directory owned by Stack.
func (s *Stack) Stop(ctx context.Context) error {
	var stopErr error
	s.stopOnce.Do(func() {
		close(s.stopCh)
		select {
		case <-s.doneCh:
		case <-time.After(3 * time.Second):
		}
		s.mu.Lock()
		for _, vs := range s.vehicles {
			if vs.conn != nil {
				_ = vs.conn.Close()
			}
		}
		s.mu.Unlock()
		s.rxWg.Wait()
		if s.rt != nil {
			stopErr = s.rt.Stop(ctx)
		}
		if s.ownedTmpDir != "" {
			_ = os.RemoveAll(s.ownedTmpDir)
		}
	})
	return stopErr
}

func (s *Stack) runLoop(interval time.Duration) {
	defer close(s.doneCh)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	dt := interval.Seconds()
	tick := 0
	for {
		select {
		case <-s.stopCh:
			return
		case now := <-ticker.C:
			tick++
			includeHB := tick%10 == 0
			s.mu.Lock()
			s.stepSimulationLocked(now, dt)
			_ = s.sendAllTelemetryLocked(includeHB)
			s.mu.Unlock()
		}
	}
}

func (s *Stack) stepSimulationLocked(now time.Time, dt float64) {
	for _, sysid := range []uint8{1, 2, 3} {
		vs := s.vehicles[sysid]
		if vs == nil {
			continue
		}
		if !vs.blackoutUntil.IsZero() && !now.Before(vs.blackoutUntil) {
			vs.blackoutUntil = time.Time{}
			vs.flags.CompanionTrunkHealthy = true
			if vs.authority == mavcodec.AuthorityMCUFailsafe {
				vs.authority = mavcodec.AuthorityCompanionTeleop
				vs.mode = mavcodec.ModeHoldPosition
			}
			_ = s.sendVehicleHeartbeatLocked(vs)
		}
		if vs.teleopActive && now.Sub(vs.lastTeleopAt) > 300*time.Millisecond {
			vs.teleopActive = false
			vs.cmdVel = [3]float64{}
			vs.yawRateDegS = 0
			vs.flags.TeleopDeadmanClamped = true
			recomputeEffectiveVelLocked(vs)
			_ = s.sendVehicleHeartbeatLocked(vs)
		}
		if vs.teleopControlled && dt > 0 {
			vs.ned[0] += vs.vel[0] * dt
			vs.ned[1] += vs.vel[1] * dt
			vs.ned[2] += vs.vel[2] * dt
			vs.yawDeg = math.Mod(math.Mod(vs.yawDeg+vs.yawRateDegS*dt, 360.0)+360.0, 360.0)
		}
	}
}

func recomputeEffectiveVelLocked(vs *vehicleSimState) {
	if !vs.armed {
		vs.vel = [3]float64{}
		return
	}
	v := vs.cmdVel
	if vs.hasInjectedCurrent {
		v[0] += vs.crossCurrentNED[0]
		v[1] += vs.crossCurrentNED[1]
	}
	vs.vel = v
}

func (s *Stack) readVehicleLoop(sysid uint8, conn *net.UDPConn) {
	defer s.rxWg.Done()
	buf := make([]byte, 512)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				return
			}
		}
		frame, decErr := mavcodec.DecodeFrame(buf[:n])
		if decErr != nil {
			continue
		}
		s.mu.Lock()
		vs := s.vehicles[sysid]
		if vs != nil {
			s.handleOutboundFrameLocked(vs, frame, time.Now())
		}
		s.mu.Unlock()
	}
}

func (s *Stack) handleOutboundFrameLocked(vs *vehicleSimState, frame mavcodec.Frame, now time.Time) {
	switch msg := frame.Message.(type) {
	case mavcodec.Heartbeat:
		if frame.SystemID == mavcodec.GCSSystemID && msg.Type == mavcodec.MAVTypeGCS {
			vs.gcsHeartbeatCount++
		}

	case mavcodec.SetPositionTargetLocalNED:
		if msg.TargetSystem != vs.sysid {
			return
		}
		if !vs.armed || vs.authority != mavcodec.AuthorityCompanionTeleop || vs.linkTier == mavcodec.LinkTierMeshSupervisory {
			return
		}
		vx := float64(msg.Vx)
		vy := float64(msg.Vy)
		vz := float64(msg.Vz)
		yawRate := float64(msg.YawRate)
		if math.IsNaN(vx) || math.IsInf(vx, 0) ||
			math.IsNaN(vy) || math.IsInf(vy, 0) ||
			math.IsNaN(vz) || math.IsInf(vz, 0) ||
			math.IsNaN(yawRate) || math.IsInf(yawRate, 0) {
			return
		}
		vN, vE := vx, vy
		if msg.CoordinateFrame == mavcodec.MAVFrameBodyNED {
			yawRad := vs.yawDeg * math.Pi / 180.0
			cy, sy := math.Cos(yawRad), math.Sin(yawRad)
			vN = vx*cy - vy*sy
			vE = vx*sy + vy*cy
		}
		vs.teleopControlled = true
		vs.mode = mavcodec.ModeTeleopVelocity
		vs.cmdVel = [3]float64{vN, vE, vz}
		vs.yawRateDegS = yawRate * 180.0 / math.Pi
		vs.flags.TeleopDeadmanClamped = false
		vs.lastTeleopAt = now
		vs.teleopActive = math.Abs(vN) > 1e-4 || math.Abs(vE) > 1e-4 || math.Abs(vz) > 1e-4 || math.Abs(yawRate) > 1e-4
		recomputeEffectiveVelLocked(vs)

	case mavcodec.CommandLong:
		if msg.TargetSystem != vs.sysid {
			return
		}
		result := s.applyVehicleCommandLocked(vs, msg, now)
		if vs.dropAckCount > 0 {
			vs.dropAckCount--
			vs.suppressHeartbeatUntil = now.Add(1900 * time.Millisecond)
			return
		}
		_ = s.sendFrameLocked(vs, mavcodec.CommandAck{
			Command:         msg.Command,
			Result:          result,
			TargetSystem:    frame.SystemID,
			TargetComponent: frame.ComponentID,
		})
		if result == mavcodec.MAVResultAccepted {
			_ = s.sendVehicleHeartbeatLocked(vs)
		}
	}
}

func (s *Stack) applyVehicleCommandLocked(vs *vehicleSimState, msg mavcodec.CommandLong, now time.Time) uint8 {
	switch msg.Command {
	case mavcodec.MAVCmdComponentArmDisarm:
		if vs.authority == mavcodec.AuthorityDirectCRSFOverride {
			return mavcodec.MAVResultDenied
		}
		if msg.Param1 >= 0.5 {
			if vs.authority == mavcodec.AuthorityMCUFailsafe {
				return mavcodec.MAVResultTemporarilyRejected
			}
			vs.armed = true
			if vs.mode == mavcodec.ModeDisarmed {
				vs.mode = mavcodec.ModeTeleopVelocity
			}
			vs.authority = mavcodec.AuthorityCompanionTeleop
			vs.flags.TeleopDeadmanClamped = false
			return mavcodec.MAVResultAccepted
		}
		vs.armed = false
		vs.mode = mavcodec.ModeDisarmed
		vs.teleopControlled = true
		vs.teleopActive = false
		vs.cmdVel = [3]float64{}
		vs.yawRateDegS = 0
		if vs.authority == mavcodec.AuthorityMCUFailsafe {
			vs.authority = mavcodec.AuthorityCompanionTeleop
			vs.flags.CompanionTrunkHealthy = true
		}
		recomputeEffectiveVelLocked(vs)
		return mavcodec.MAVResultAccepted

	case mavcodec.MAVCmdNavLoiterUnlim:
		if vs.authority == mavcodec.AuthorityDirectCRSFOverride || vs.authority == mavcodec.AuthorityMCUFailsafe {
			return mavcodec.MAVResultTemporarilyRejected
		}
		vs.mode = mavcodec.ModeHoldPosition
		vs.teleopControlled = true
		vs.teleopActive = false
		vs.cmdVel = [3]float64{}
		vs.yawRateDegS = 0
		recomputeEffectiveVelLocked(vs)
		return mavcodec.MAVResultAccepted

	case mavcodec.MAVCmdDoSetMode:
		if vs.authority == mavcodec.AuthorityDirectCRSFOverride || vs.authority == mavcodec.AuthorityMCUFailsafe {
			return mavcodec.MAVResultTemporarilyRejected
		}
		switch mavcodec.CompanionMode(math.Round(float64(msg.Param2))) {
		case mavcodec.ModeTeleopVelocity:
			vs.mode = mavcodec.ModeTeleopVelocity
			vs.authority = mavcodec.AuthorityCompanionTeleop
			vs.flags.TeleopDeadmanClamped = false
			return mavcodec.MAVResultAccepted
		case mavcodec.ModeAutonomousVelocity:
			vs.mode = mavcodec.ModeAutonomousVelocity
			vs.authority = mavcodec.AuthorityCompanionAutonomous
			vs.teleopActive = false
			return mavcodec.MAVResultAccepted
		case mavcodec.ModeHoldPosition:
			vs.mode = mavcodec.ModeHoldPosition
			vs.teleopControlled = true
			vs.teleopActive = false
			vs.cmdVel = [3]float64{}
			vs.yawRateDegS = 0
			recomputeEffectiveVelLocked(vs)
			return mavcodec.MAVResultAccepted
		default:
			return mavcodec.MAVResultUnsupported
		}

	case mavcodec.MAVCmdUser1:
		if vs.linkTier != mavcodec.LinkTierSITLLocal {
			return mavcodec.MAVResultDenied
		}
		switch int(math.Round(float64(msg.Param1))) {
		case 1: // Simulated companion trunk blackout (default 350ms -> MCU failsafe).
			durMs := float64(msg.Param2)
			if durMs <= 0 {
				durMs = 350
			}
			if durMs > 2000 {
				durMs = 2000
			}
			vs.authority = mavcodec.AuthorityMCUFailsafe
			vs.mode = mavcodec.ModeHoldPosition
			vs.flags.CompanionTrunkHealthy = false
			vs.flags.TeleopDeadmanClamped = true
			vs.failsafeCount++
			vs.teleopControlled = true
			vs.teleopActive = false
			vs.cmdVel = [3]float64{}
			vs.yawRateDegS = 0
			vs.blackoutUntil = now.Add(time.Duration(durMs) * time.Millisecond)
			recomputeEffectiveVelLocked(vs)
			return mavcodec.MAVResultAccepted

		case 2: // Simulated P1 CRSF override set / restore / toggle.
			enableCRSF := msg.Param2 >= 0.5 || (math.Abs(float64(msg.Param2)) < 0.5 && vs.authority != mavcodec.AuthorityDirectCRSFOverride)
			if enableCRSF {
				vs.authority = mavcodec.AuthorityDirectCRSFOverride
				vs.mode = mavcodec.ModeHoldPosition
				vs.flags.TeleopDeadmanClamped = true
				vs.teleopActive = false
				vs.cmdVel = [3]float64{}
				vs.yawRateDegS = 0
				if vs.teleopControlled {
					recomputeEffectiveVelLocked(vs)
				}
			} else {
				vs.authority = mavcodec.AuthorityCompanionTeleop
				vs.mode = mavcodec.ModeTeleopVelocity
				vs.flags.TeleopDeadmanClamped = false
			}
			return mavcodec.MAVResultAccepted

		case 3: // Simulated cross-current vector (m/s North, m/s East).
			vs.crossCurrentNED = [2]float64{float64(msg.Param2), float64(msg.Param3)}
			vs.hasInjectedCurrent = true
			vs.teleopControlled = true
			recomputeEffectiveVelLocked(vs)
			return mavcodec.MAVResultAccepted

		default:
			return mavcodec.MAVResultUnsupported
		}

	default:
		return mavcodec.MAVResultUnsupported
	}
}

func (s *Stack) sendAllTelemetryLocked(includeHeartbeat bool) error {
	now := time.Now()
	origin := [3]float64{defaultOriginLatDeg, defaultOriginLonDeg, defaultOriginAltM}
	for _, sysid := range []uint8{1, 2, 3} {
		vs := s.vehicles[sysid]
		if vs == nil {
			continue
		}
		emitHB := includeHeartbeat
		if !vs.suppressHeartbeatUntil.IsZero() {
			if now.Before(vs.suppressHeartbeatUntil) {
				emitHB = false
			} else {
				vs.suppressHeartbeatUntil = time.Time{}
				emitHB = true
			}
		}
		if emitHB {
			if err := s.sendVehicleHeartbeatLocked(vs); err != nil {
				return err
			}
		}
		if err := s.sendFrameLocked(vs, mavcodec.SysStatus{
			Load:         820,
			ErrorsComm:   0,
			ErrorsCount1: vs.failsafeCount,
			ErrorsCount2: 1100,
			ErrorsCount3: 420,
		}); err != nil {
			return err
		}
		quat := EulerZYXDegToQuaternion(vs.rollDeg, vs.pitchDeg, vs.yawDeg)
		if err := s.sendFrameLocked(vs, mavcodec.AttitudeQuaternion{
			Q1:       quat[0],
			Q2:       quat[1],
			Q3:       quat[2],
			Q4:       quat[3],
			Yawspeed: float32(vs.yawRateDegS * math.Pi / 180.0),
		}); err != nil {
			return err
		}
		if err := s.sendFrameLocked(vs, mavcodec.LocalPositionNED{
			X:  float32(vs.ned[0]),
			Y:  float32(vs.ned[1]),
			Z:  float32(vs.ned[2]),
			Vx: float32(vs.vel[0]),
			Vy: float32(vs.vel[1]),
			Vz: float32(vs.vel[2]),
		}); err != nil {
			return err
		}
		lla, err := LocalNEDToLLA(vs.ned, origin)
		if err != nil {
			return err
		}
		hdgCdeg := uint16(math.Mod(math.Mod(vs.yawDeg, 360.0)+360.0, 360.0) * 100.0)
		if err := s.sendFrameLocked(vs, mavcodec.GlobalPositionInt{
			Lat:         int32(math.Round(lla[0] * 1e7)),
			Lon:         int32(math.Round(lla[1] * 1e7)),
			Alt:         int32(math.Round(lla[2] * 1000.0)),
			RelativeAlt: int32(math.Round(-vs.ned[2] * 1000.0)),
			Hdg:         hdgCdeg,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Stack) sendVehicleHeartbeatLocked(vs *vehicleSimState) error {
	hb, err := mavcodec.EncodeVehicleHeartbeat(mavcodec.VehicleHeartbeat{
		Armed: vs.armed,
		Custom: mavcodec.CustomMode{
			Domain:           vs.domain,
			ControlAuthority: vs.authority,
			CompanionMode:    vs.mode,
			LinkTier:         vs.linkTier,
			HealthFlags:      vs.flags,
		},
	})
	if err != nil {
		return err
	}
	return s.sendFrameLocked(vs, hb)
}

func (s *Stack) sendFrameLocked(vs *vehicleSimState, msg mavcodec.Message) error {
	vs.seq++
	raw, err := mavcodec.EncodeFrame(mavcodec.Frame{
		Sequence:    vs.seq,
		SystemID:    vs.sysid,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message:     msg,
	})
	if err != nil {
		return err
	}
	_, err = vs.conn.WriteToUDP(raw, s.serverUDP)
	return err
}

func (s *Stack) waitInitialConvergence(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	kv, err := s.rt.NATSClient().GetKeyValueBucket(waitCtx, graph.BucketEntityStates)
	if err != nil {
		return fmt.Errorf("e2eharness: lookup ENTITY_STATES: %w", err)
	}

	requiredKeys := []string{
		s.info.Vehicles.Quadcopter,
		s.info.Vehicles.Rover,
		s.info.Vehicles.SurfaceBoat,
		s.info.ExternalContactID,
		s.info.CorridorEntityID,
	}

	for {
		allPresent := true
		for _, key := range requiredKeys {
			if _, getErr := kv.Get(waitCtx, key); getErr != nil {
				allPresent = false
				break
			}
		}
		if allPresent && s.isTwoHopPathIndexed(waitCtx) {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("e2eharness: timed out awaiting ENTITY_STATES + pathSearch convergence: %w", waitCtx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *Stack) isTwoHopPathIndexed(ctx context.Context) bool {
	reqBody, err := json.Marshal(map[string]any{
		"query": `query($start: String!) { pathSearch(startEntity: $start, direction: "both", maxDepth: 2, maxNodes: 50) { entities { id depth } truncated } }`,
		"variables": map[string]any{
			"start": s.info.Vehicles.Quadcopter,
		},
	})
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.info.BaseURL+"/graph-gateway/graphql", bytes.NewReader(reqBody))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	var decoded struct {
		Data struct {
			PathSearch *struct {
				Entities []struct {
					ID    string `json:"id"`
					Depth int    `json:"depth"`
				} `json:"entities"`
			} `json:"pathSearch"`
		} `json:"data"`
		Errors []struct{ Message string } `json:"errors"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Errors) > 0 || decoded.Data.PathSearch == nil {
		return false
	}
	seen := make(map[string]bool, len(decoded.Data.PathSearch.Entities))
	for _, n := range decoded.Data.PathSearch.Entities {
		seen[n.ID] = true
	}
	return seen[s.info.Vehicles.Quadcopter] && seen[s.info.ExternalContactID] && seen[s.info.CorridorEntityID]
}
