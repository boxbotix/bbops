package uxsvocab

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/c360studio/semengine/graph"
	"github.com/c360studio/semengine/message"
	"github.com/c360studio/semengine/payloadregistry"
	semtypes "github.com/c360studio/semengine/pkg/types"
	"github.com/c360studio/semengine/vocabulary"
)

// spec: mavlink-ingest / UXS consumer vocabulary pack and ADR-102 vehicle entity identity
func TestVocabularyPackAndPredicates(t *testing.T) {
	wantPredicates := []struct {
		name     string
		dataType string
	}{
		{name: "uxs.domain.kind", dataType: "string"},
		{name: "uxs.authority.state", dataType: "string"},
		{name: "uxs.mode.companion", dataType: "string"},
		{name: "uxs.link.tier", dataType: "string"},
		{name: "uxs.health.armed", dataType: "bool"},
		{name: "uxs.health.flags", dataType: "json"},
		{name: "uxs.health.failsafes", dataType: "int"},
		{name: "uxs.health.crcerrors", dataType: "int"},
		{name: "uxs.health.loopdtus", dataType: "int"},
		{name: "uxs.health.maxloopdtus", dataType: "int"},
		{name: "uxs.health.rttus", dataType: "int"},
		{name: "uxs.attitude.quaternion", dataType: "json"},
		{name: "uxs.attitude.rates", dataType: "json"},
		{name: "uxs.kinematics.ned", dataType: "json"},
		{name: "uxs.kinematics.vx", dataType: "float"},
		{name: "uxs.kinematics.vy", dataType: "float"},
		{name: "uxs.kinematics.vz", dataType: "float"},
		{name: "uxs.kinematics.headingdeg", dataType: "float"},
	}

	allowedDataTypes := map[string]bool{
		vocabulary.DataTypeString:   true,
		vocabulary.DataTypeEntityID: true,
		vocabulary.DataTypeInt:      true,
		vocabulary.DataTypeFloat:    true,
		vocabulary.DataTypeBool:     true,
		vocabulary.DataTypeDateTime: true,
		vocabulary.DataTypeJSON:     true,
	}

	pack, err := LoadPack()
	if err != nil {
		t.Fatalf("LoadPack: %v", err)
	}
	if pack.Name != PackName {
		t.Fatalf("pack.Name = %q, want %q", pack.Name, PackName)
	}
	if len(pack.Predicates) != len(wantPredicates) {
		t.Fatalf("len(pack.Predicates) = %d, want %d", len(pack.Predicates), len(wantPredicates))
	}

	gotNames := UXSPredicates()
	if len(gotNames) != len(wantPredicates) {
		t.Fatalf("len(UXSPredicates()) = %d, want %d", len(gotNames), len(wantPredicates))
	}

	for i, want := range wantPredicates {
		spec := pack.Predicates[i]
		if spec.Name != want.name {
			t.Fatalf("pack.Predicates[%d].Name = %q, want %q", i, spec.Name, want.name)
		}
		if gotNames[i] != want.name {
			t.Fatalf("UXSPredicates()[%d] = %q, want %q", i, gotNames[i], want.name)
		}
		if spec.DataType != want.dataType {
			t.Fatalf("predicate %q DataType = %q, want %q", spec.Name, spec.DataType, want.dataType)
		}
		if !allowedDataTypes[spec.DataType] {
			t.Fatalf("predicate %q DataType %q outside SemEngine closed 7-datatype set", spec.Name, spec.DataType)
		}
		if strings.Contains(spec.Name, "_") || spec.Name != strings.ToLower(spec.Name) {
			t.Fatalf("predicate %q contains underscore or uppercase characters", spec.Name)
		}
		if parts := strings.Split(spec.Name, "."); len(parts) != 3 {
			t.Fatalf("predicate %q has %d segments, want 3", spec.Name, len(parts))
		}
	}

	if len(pack.EntityTypes) != 1 || pack.EntityTypes[0].DomainType != EntityTypeVehicle {
		t.Fatalf("pack.EntityTypes = %+v, want [%s]", pack.EntityTypes, EntityTypeVehicle)
	}
	if VehicleDomainDelegation.Producer != "bbops" || VehicleDomainDelegation.Domain != "uxs" || VehicleDomainDelegation.Type != "vehicle" {
		t.Fatalf("VehicleDomainDelegation = %+v, want {bbops uxs vehicle}", VehicleDomainDelegation)
	}

	// Idempotent global registration.
	if err := Register(); err != nil {
		t.Fatalf("Register (first): %v", err)
	}
	if err := Register(); err != nil {
		t.Fatalf("Register (second idempotent call): %v", err)
	}

	for _, want := range wantPredicates {
		meta := vocabulary.GetPredicateMetadata(want.name)
		if meta == nil {
			t.Fatalf("GetPredicateMetadata(%q) returned nil after Register()", want.name)
		}
		if meta.DataType != want.dataType {
			t.Fatalf("registered %q DataType = %q, want %q", want.name, meta.DataType, want.dataType)
		}
	}
	entityMeta := vocabulary.GetEntityTypeMetadata(EntityTypeVehicle)
	if entityMeta == nil || entityMeta.Domain != "uxs" || entityMeta.Type != "vehicle" {
		t.Fatalf("GetEntityTypeMetadata(%q) = %+v, want uxs.vehicle", EntityTypeVehicle, entityMeta)
	}

	// Reject malformed JSON, unknown fields, and underscore predicate names.
	invalidPacks := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "unknown field", raw: `{"name":"bad","unknown":1}`},
		{name: "underscore predicate", raw: `{"name":"bad","predicates":[{"name":"uxs.health.loop_dt","data_type":"int"}]}`},
		{name: "invalid datatype", raw: `{"name":"bad","predicates":[{"name":"uxs.health.loopdt","data_type":"double"}]}`},
	}
	for _, tc := range invalidPacks {
		if _, err := loadPackBytes([]byte(tc.raw)); err == nil {
			t.Fatalf("loadPackBytes(%s) succeeded, want error", tc.name)
		}
	}
}

// spec: mavlink-ingest / UXS consumer vocabulary pack and ADR-102 vehicle entity identity
func TestVehicleEntityID(t *testing.T) {
	for _, sysid := range []uint8{1, 2, 42, 254} {
		got, err := VehicleEntityID("boxbotix", "gcs1", sysid)
		if err != nil {
			t.Fatalf("VehicleEntityID(sysid=%d): %v", sysid, err)
		}
		if err := semtypes.ValidateEntityID(got); err != nil {
			t.Fatalf("ValidateEntityID(%q): %v", got, err)
		}
		parsed, err := semtypes.ParseEntityID(got)
		if err != nil {
			t.Fatalf("ParseEntityID(%q): %v", got, err)
		}
		if parsed.Org != "boxbotix" || parsed.Platform != "gcs1" || parsed.System != "robotics" || parsed.Domain != "uxs" || parsed.Type != "vehicle" {
			t.Fatalf("parsed segments = %+v", parsed)
		}
	}

	// Exact 256-byte boundary succeeds; 257-byte input fails.
	// Fixed suffix ".gcs1.robotics.uxs.vehicle.254" is 30 bytes, leaving 226 bytes for org.
	org226 := strings.Repeat("a", 226)
	id256, err := VehicleEntityID(org226, "gcs1", 254)
	if err != nil {
		t.Fatalf("256-byte boundary VehicleEntityID failed: %v", err)
	}
	if len(id256) != semtypes.MaxEntityIDBytes {
		t.Fatalf("len(id256) = %d, want %d", len(id256), semtypes.MaxEntityIDBytes)
	}
	if _, err := VehicleEntityID(org226+"b", "gcs1", 254); err == nil {
		t.Fatal("257-byte VehicleEntityID succeeded, want error")
	}

	invalidCases := []struct {
		name     string
		org      string
		platform string
		sysid    uint8
	}{
		{name: "sysid 0 broadcast", org: "boxbotix", platform: "gcs1", sysid: 0},
		{name: "sysid 255 gcs", org: "boxbotix", platform: "gcs1", sysid: 255},
		{name: "empty org", org: "", platform: "gcs1", sysid: 1},
		{name: "empty platform", org: "boxbotix", platform: "", sysid: 1},
		{name: "dotted org", org: "box.botix", platform: "gcs1", sysid: 1},
		{name: "dotted platform", org: "boxbotix", platform: "gcs.1", sysid: 1},
		{name: "leading hyphen org", org: "-boxbotix", platform: "gcs1", sysid: 1},
		{name: "whitespace platform", org: "boxbotix", platform: "gcs 1", sysid: 1},
	}
	for _, tc := range invalidCases {
		if got, err := VehicleEntityID(tc.org, tc.platform, tc.sysid); err == nil {
			t.Fatalf("%s: VehicleEntityID returned %q without error", tc.name, got)
		}
	}
}

// spec: mavlink-ingest / Constant-predicate Graphable vehicle snapshot payload
func TestVehicleSnapshotConstantPredicatesAndValidation(t *testing.T) {
	wantOrder := SnapshotPredicates()
	if len(wantOrder) != 22 {
		t.Fatalf("len(SnapshotPredicates()) = %d, want 22", len(wantOrder))
	}

	for _, domain := range []string{DomainQuadcopter, DomainRover, DomainSurfaceBoat} {
		initial, err := NewVehicleSnapshot("boxbotix", "alpha", 7, domain)
		if err != nil {
			t.Fatalf("NewVehicleSnapshot(%s): %v", domain, err)
		}
		if err := initial.Validate(); err != nil {
			t.Fatalf("initial.Validate(%s): %v", domain, err)
		}

		populated := &VehicleSnapshot{
			Org:           "boxbotix",
			Platform:      "alpha",
			SysID:         7,
			Title:         "USV- Osprey ",
			Domain:        domain,
			Authority:     AuthorityCompanionTeleop,
			CompanionMode: ModeTeleopVelocity,
			LinkTier:      LinkTierMeshHighRate,
			Armed:         true,
			Flags: HealthFlags{
				ClockSyncLocked:       true,
				TeleopDeadmanClamped:  false,
				CRSFLinkHealthy:       true,
				CompanionTrunkHealthy: true,
			},
			Failsafes:    1,
			CRCErrors:    2,
			LoopDtUs:     1000,
			MaxLoopDtUs:  1450,
			RTTUs:        820,
			Quaternion:   [4]float64{0.70710678, 0, 0, 0.70710678},
			BodyRates:    [3]float64{0.01, -0.02, 0.15},
			NED:          [3]float64{12.5, -4.25, -1.5},
			Vx:           2.4,
			Vy:           -0.6,
			Vz:           0.0,
			HeadingDeg:   360.0,
			Latitude:     -90.0,
			Longitude:    180.0,
			Altitude:     142.75,
			ObservedAt:   time.Date(2026, 11, 23, 21, 0, 0, 0, time.UTC),
			PoseMetadata: true,
			HasAttitude:  true,
			HasLocalPos:  true,
			HasGlobalPos: true,
			HasHeading:   true,
		}
		if err := populated.Validate(); err != nil {
			t.Fatalf("populated.Validate(%s): %v", domain, err)
		}

		initTriples := initial.Triples()
		popTriples := populated.Triples()
		if len(initTriples) != 22 || len(popTriples) != 22 {
			t.Fatalf("triples len = (%d, %d), want (22, 22)", len(initTriples), len(popTriples))
		}

		var gotInitPreds, gotPopPreds []string
		for i := range 22 {
			gotInitPreds = append(gotInitPreds, initTriples[i].Predicate)
			gotPopPreds = append(gotPopPreds, popTriples[i].Predicate)
			for _, tr := range []any{initTriples[i], popTriples[i]} {
				_ = tr
			}
			if initTriples[i].Source == graph.SourceIndexingProfile || popTriples[i].Source == graph.SourceIndexingProfile {
				t.Fatal("snapshot emitted reserved graph.SourceIndexingProfile")
			}
			if initTriples[i].Predicate == vocabulary.EntityIndexingProfile || popTriples[i].Predicate == vocabulary.EntityIndexingProfile {
				t.Fatal("snapshot emitted entity.indexing.profile directly instead of payload registration floor")
			}
		}
		if !slices.Equal(gotInitPreds, wantOrder) {
			t.Fatalf("initial predicates = %v, want %v", gotInitPreds, wantOrder)
		}
		if !slices.Equal(gotPopPreds, wantOrder) {
			t.Fatalf("populated predicates = %v, want %v", gotPopPreds, wantOrder)
		}

		// Verify D2 Option A: DataTypeJSON objects remain native Go structs/arrays.
		if _, ok := popTriples[5].Object.(HealthFlags); !ok {
			t.Fatalf("uxs.health.flags Object type = %T, want HealthFlags", popTriples[5].Object)
		}
		if _, ok := popTriples[11].Object.([4]float64); !ok {
			t.Fatalf("uxs.attitude.quaternion Object type = %T, want [4]float64", popTriples[11].Object)
		}
		if _, ok := popTriples[12].Object.([3]float64); !ok {
			t.Fatalf("uxs.attitude.rates Object type = %T, want [3]float64", popTriples[12].Object)
		}
		if _, ok := popTriples[13].Object.([3]float64); !ok {
			t.Fatalf("uxs.kinematics.ned Object type = %T, want [3]float64", popTriples[13].Object)
		}

		// Birth snapshot marks unmeasured pose triples [11..20] with Confidence=0 and epoch-0 timestamp,
		// while populated snapshot marks measured pose triples with Confidence=1 and Context="measured".
		epochZero := time.Unix(0, 0).UTC()
		for idx := 11; idx <= 20; idx++ {
			if initTriples[idx].Confidence != 0 || !initTriples[idx].Timestamp.Equal(epochZero) || initTriples[idx].Context != "" {
				t.Fatalf("initial pose triple[%d] (%s) = %+v, want Confidence=0 Timestamp=epoch0", idx, initTriples[idx].Predicate, initTriples[idx])
			}
			if popTriples[idx].Confidence != 1.0 || popTriples[idx].Context != "measured" {
				t.Fatalf("populated pose triple[%d] (%s) = %+v, want Confidence=1 Context=measured", idx, popTriples[idx].Predicate, popTriples[idx])
			}
		}

		// Health-only ObservedAt advancement must not overwrite earlier per-domain pose timestamps.
		poseTS := time.Date(2026, 11, 23, 20, 55, 0, 0, time.UTC)
		healthLater := *populated
		healthLater.LocalPosObservedUnixNs = poseTS.UnixNano()
		healthLater.ObservedAt = poseTS.Add(10 * time.Minute)
		laterTriples := healthLater.Triples()
		if !laterTriples[4].Timestamp.Equal(healthLater.ObservedAt) {
			t.Fatalf("health triple timestamp = %v, want %v", laterTriples[4].Timestamp, healthLater.ObservedAt)
		}
		if !laterTriples[13].Timestamp.Equal(poseTS) {
			t.Fatalf("uxs.kinematics.ned timestamp = %v, want preserved %v", laterTriples[13].Timestamp, poseTS)
		}

		// JSON Marshal/Unmarshal round-trip.
		encoded, err := json.Marshal(populated)
		if err != nil {
			t.Fatalf("Marshal VehicleSnapshot: %v", err)
		}
		var roundTrip VehicleSnapshot
		if err := json.Unmarshal(encoded, &roundTrip); err != nil {
			t.Fatalf("Unmarshal VehicleSnapshot: %v", err)
		}
		if !reflect.DeepEqual(&roundTrip, populated) {
			t.Fatalf("round-trip mismatch:\ngot:  %+v\nwant: %+v", roundTrip, *populated)
		}
	}

	// Boundary and invalid snapshot cases.
	var nilSnap *VehicleSnapshot
	if nilSnap.Validate() == nil || nilSnap.EntityID() != "" || nilSnap.Triples() != nil {
		t.Fatal("nil snapshot did not fail/return empty cleanly")
	}
	if _, err := NewVehicleSnapshot("boxbotix", "alpha", 1, "submarine"); err == nil {
		t.Fatal("NewVehicleSnapshot accepted invalid domain")
	}
	if _, err := NewVehicleSnapshot("boxbotix", "alpha", 0, DomainQuadcopter); err == nil {
		t.Fatal("NewVehicleSnapshot accepted sysid 0")
	}

	baseValid := func() *VehicleSnapshot {
		s, err := NewVehicleSnapshot("boxbotix", "alpha", 3, DomainRover)
		if err != nil {
			t.Fatalf("baseValid: %v", err)
		}
		return s
	}

	// Valid exact boundaries: heading=0, lat=90, lon=-180.
	boundary := baseValid()
	boundary.HeadingDeg = 0
	boundary.Latitude = 90
	boundary.Longitude = -180
	boundary.Title = "   "
	if err := boundary.Validate(); err != nil {
		t.Fatalf("exact boundary Validate failed: %v", err)
	}
	if gotTitle := boundary.Triples()[21].Object; gotTitle != "UXS-3" {
		t.Fatalf("blank title fallback = %v, want UXS-3", gotTitle)
	}

	mutators := []struct {
		name   string
		mutate func(*VehicleSnapshot)
	}{
		{name: "bad sysid", mutate: func(s *VehicleSnapshot) { s.SysID = 255 }},
		{name: "bad domain", mutate: func(s *VehicleSnapshot) { s.Domain = "fixed_wing" }},
		{name: "bad authority", mutate: func(s *VehicleSnapshot) { s.Authority = "manual" }},
		{name: "bad mode", mutate: func(s *VehicleSnapshot) { s.CompanionMode = "rtl" }},
		{name: "bad link tier", mutate: func(s *VehicleSnapshot) { s.LinkTier = "satcom" }},
		{name: "negative failsafes", mutate: func(s *VehicleSnapshot) { s.Failsafes = -1 }},
		{name: "negative crc errors", mutate: func(s *VehicleSnapshot) { s.CRCErrors = -1 }},
		{name: "negative loop dt", mutate: func(s *VehicleSnapshot) { s.LoopDtUs = -1 }},
		{name: "negative max loop dt", mutate: func(s *VehicleSnapshot) { s.MaxLoopDtUs = -1 }},
		{name: "negative rtt", mutate: func(s *VehicleSnapshot) { s.RTTUs = -1 }},
		{name: "NaN quaternion", mutate: func(s *VehicleSnapshot) { s.Quaternion[2] = math.NaN() }},
		{name: "+Inf body rate", mutate: func(s *VehicleSnapshot) { s.BodyRates[1] = math.Inf(1) }},
		{name: "-Inf NED", mutate: func(s *VehicleSnapshot) { s.NED[0] = math.Inf(-1) }},
		{name: "NaN vx", mutate: func(s *VehicleSnapshot) { s.Vx = math.NaN() }},
		{name: "+Inf vy", mutate: func(s *VehicleSnapshot) { s.Vy = math.Inf(1) }},
		{name: "-Inf vz", mutate: func(s *VehicleSnapshot) { s.Vz = math.Inf(-1) }},
		{name: "NaN altitude", mutate: func(s *VehicleSnapshot) { s.Altitude = math.NaN() }},
		{name: "heading < 0", mutate: func(s *VehicleSnapshot) { s.HeadingDeg = -0.01 }},
		{name: "heading > 360", mutate: func(s *VehicleSnapshot) { s.HeadingDeg = 360.01 }},
		{name: "lat < -90", mutate: func(s *VehicleSnapshot) { s.Latitude = -90.01 }},
		{name: "lat > 90", mutate: func(s *VehicleSnapshot) { s.Latitude = 90.01 }},
		{name: "lon < -180", mutate: func(s *VehicleSnapshot) { s.Longitude = -180.01 }},
		{name: "lon > 180", mutate: func(s *VehicleSnapshot) { s.Longitude = 180.01 }},
	}
	for _, tc := range mutators {
		s := baseValid()
		tc.mutate(s)
		if err := s.Validate(); err == nil {
			t.Fatalf("Validate(%s) succeeded, want error", tc.name)
		}
	}

	if err := RegisterPayloads(nil); err == nil {
		t.Fatal("RegisterPayloads(nil) succeeded, want error")
	}
	reg := payloadregistry.New()
	if err := RegisterPayloads(reg); err != nil {
		t.Fatalf("RegisterPayloads: %v", err)
	}
	if err := RegisterPayloads(reg); err == nil {
		t.Fatal("duplicate RegisterPayloads succeeded, want error")
	}
}

// spec: mavlink-ingest / Constant-predicate Graphable vehicle snapshot payload
func TestVehicleSnapshotJSONVectorDimensionsViaRegisteredDecoder(t *testing.T) {
	reg := payloadregistry.New()
	if err := RegisterPayloads(reg); err != nil {
		t.Fatalf("RegisterPayloads: %v", err)
	}
	dec := message.NewDecoder(reg)

	valid := &VehicleSnapshot{
		Org:           "boxbotix",
		Platform:      "gcs01",
		SysID:         7,
		Title:         "ROVER-7",
		Domain:        DomainRover,
		Authority:     AuthorityCompanionTeleop,
		CompanionMode: ModeTeleopVelocity,
		LinkTier:      LinkTierMeshHighRate,
		Armed:         true,
		Quaternion:    [4]float64{1, 0, 0, 0},
		BodyRates:     [3]float64{0.1, -0.2, 0.3},
		NED:           [3]float64{10.5, -4.25, 0.0},
		HeadingDeg:    90.0,
		Latitude:      37.7749,
		Longitude:     -122.4194,
		Altitude:      12.5,
		ObservedAt:    time.Date(2026, 11, 23, 18, 0, 0, 0, time.UTC),
	}

	validEnvelope, err := json.Marshal(message.NewBaseMessage(valid.Schema(), valid, "decoder-test"))
	if err != nil {
		t.Fatalf("marshal valid BaseMessage: %v", err)
	}
	decodedMsg, err := dec.Decode(validEnvelope)
	if err != nil {
		t.Fatalf("dec.Decode(validEnvelope): %v", err)
	}
	gotSnap, ok := decodedMsg.Payload().(*VehicleSnapshot)
	if !ok {
		t.Fatalf("decoded payload = %T, want *VehicleSnapshot", decodedMsg.Payload())
	}
	if gotSnap.Quaternion != valid.Quaternion || gotSnap.BodyRates != valid.BodyRates || gotSnap.NED != valid.NED {
		t.Fatalf("decoded vectors = (%v, %v, %v), want (%v, %v, %v)",
			gotSnap.Quaternion, gotSnap.BodyRates, gotSnap.NED,
			valid.Quaternion, valid.BodyRates, valid.NED)
	}

	// Numeric-zero control: explicit 0 components across all 3 tuples must succeed.
	zeroVecSnap := *valid
	zeroVecSnap.Quaternion = [4]float64{0, 0, 0, 0}
	zeroVecSnap.BodyRates = [3]float64{0, 0, 0}
	zeroVecSnap.NED = [3]float64{0, 0, 0}
	zeroEnvelope, err := json.Marshal(message.NewBaseMessage(zeroVecSnap.Schema(), &zeroVecSnap, "decoder-zero-control"))
	if err != nil {
		t.Fatalf("marshal zero-vector BaseMessage: %v", err)
	}
	zeroDecodedMsg, err := dec.Decode(zeroEnvelope)
	if err != nil {
		t.Fatalf("dec.Decode(zeroEnvelope): %v", err)
	}
	if err := zeroDecodedMsg.Validate(); err != nil {
		t.Fatalf("zeroDecodedMsg.Validate(): %v", err)
	}
	gotZero, ok := zeroDecodedMsg.Payload().(*VehicleSnapshot)
	if !ok || gotZero.Quaternion != [4]float64{} || gotZero.BodyRates != [3]float64{} || gotZero.NED != [3]float64{} {
		t.Fatalf("decoded zero-vector control = %+v, want all-zero vectors", gotZero)
	}

	// Legacy v1 global-only wire payload (no pose_metadata or has_* fields, [1,0,0,0] v1 birth quat):
	// default zero NED, [1,0,0,0] birth quat, and zero heading must NOT decode as Confidence=1 / Context="measured".
	legacyGlobalOnlyRaw := []byte(`{
		"id":"legacy-v1-global-only",
		"type":{"domain":"uxs","category":"vehicle","version":"v1"},
		"payload":{
			"org":"boxbotix","platform":"gcs01","sysid":7,"title":"ROVER-7",
			"domain":"rover","authority":"companion-teleop","companion_mode":"teleop-velocity",
			"link_tier":"mesh-high-rate","armed":true,
			"flags":{"clock_sync_locked":true,"teleop_deadman_clamped":false,"crsf_link_healthy":true,"companion_trunk_healthy":true},
			"failsafes":0,"crc_errors":0,"loop_dt_us":1000,"max_loop_dt_us":1200,"rtt_us":300,
			"quaternion":[1,0,0,0],"body_rates":[0,0,0],"ned":[0,0,0],
			"vx":0,"vy":0,"vz":0,"heading_deg":0,
			"latitude":37.7749,"longitude":-122.4194,"altitude":15.0,
			"observed_at":"2026-11-23T18:00:00Z"
		}
	}`)
	legacyMsg, err := dec.Decode(legacyGlobalOnlyRaw)
	if err != nil {
		t.Fatalf("dec.Decode(legacyGlobalOnlyRaw): %v", err)
	}
	if err := legacyMsg.Validate(); err != nil {
		t.Fatalf("legacyMsg.Validate(): %v", err)
	}
	legacySnap := legacyMsg.Payload().(*VehicleSnapshot)
	if legacySnap.PoseMetadata || legacySnap.HasLocalPos || legacySnap.HasHeading {
		t.Fatalf("legacy v1 payload decoded false measurement flags: %+v", legacySnap)
	}
	legacyByPred := make(map[string]message.Triple, 22)
	for _, tr := range legacySnap.Triples() {
		legacyByPred[tr.Predicate] = tr
	}
	for _, unmeasuredPred := range []string{
		PredicateAttitudeQuaternion,
		PredicateAttitudeRates,
		PredicateKinematicsNED,
		PredicateKinematicsVx,
		PredicateKinematicsVy,
		PredicateKinematicsVz,
		PredicateKinematicsHeadingDeg,
	} {
		tr := legacyByPred[unmeasuredPred]
		if tr.Confidence != 0 || tr.Context != "" {
			t.Fatalf("legacy v1 global-only %s = %+v, want Confidence=0 Context=\"\"", unmeasuredPred, tr)
		}
	}
	if latTr := legacyByPred[vocabulary.GeoLocationLatitude]; latTr.Confidence != 1.0 || latTr.Context != "" {
		t.Fatalf("legacy v1 global-only latitude = %+v, want Confidence=1 Context=\"\"", latTr)
	}

	// Modern wire payload with genuinely measured local zero [0,0,0] and measured 0-deg heading:
	// preserves Confidence=1 and Context="measured" across wire decode.
	measuredZeroSnap, err := NewVehicleSnapshot("boxbotix", "gcs01", 7, DomainRover)
	if err != nil {
		t.Fatalf("NewVehicleSnapshot(measuredZeroSnap): %v", err)
	}
	measuredObs := time.Date(2026, 11, 23, 18, 5, 0, 0, time.UTC)
	measuredZeroSnap.HasLocalPos = true
	measuredZeroSnap.LocalPosObservedUnixNs = measuredObs.UnixNano()
	measuredZeroSnap.HasGlobalPos = true
	measuredZeroSnap.GlobalPosObservedUnixNs = measuredObs.UnixNano()
	measuredZeroSnap.Latitude = 37.7749
	measuredZeroSnap.Longitude = -122.4194
	measuredZeroSnap.Altitude = 15.0
	measuredZeroSnap.HasHeading = true
	measuredZeroSnap.HeadingDeg = 0.0
	measuredZeroSnap.HeadingObservedUnixNs = measuredObs.UnixNano()
	measuredZeroSnap.ObservedAt = measuredObs
	measuredZeroWire, err := json.Marshal(message.NewBaseMessage(measuredZeroSnap.Schema(), measuredZeroSnap, "measured-zero"))
	if err != nil {
		t.Fatalf("marshal measuredZeroSnap: %v", err)
	}
	measuredZeroDecoded, err := dec.Decode(measuredZeroWire)
	if err != nil {
		t.Fatalf("dec.Decode(measuredZeroWire): %v", err)
	}
	if err := measuredZeroDecoded.Validate(); err != nil {
		t.Fatalf("measuredZeroDecoded.Validate(): %v", err)
	}
	measuredZeroByPred := make(map[string]message.Triple, 22)
	for _, tr := range measuredZeroDecoded.Payload().(*VehicleSnapshot).Triples() {
		measuredZeroByPred[tr.Predicate] = tr
	}
	for _, measuredPred := range []string{
		PredicateKinematicsNED,
		PredicateKinematicsHeadingDeg,
		vocabulary.GeoLocationLatitude,
	} {
		tr := measuredZeroByPred[measuredPred]
		if tr.Confidence != 1.0 || tr.Context != "measured" || !tr.Timestamp.Equal(measuredObs) {
			t.Fatalf("measured zero %s = %+v, want Confidence=1 Context=measured Timestamp=%v", measuredPred, tr, measuredObs)
		}
	}
	if attTr := measuredZeroByPred[PredicateAttitudeQuaternion]; attTr.Confidence != 0 || attTr.Context != "" {
		t.Fatalf("unmeasured attitude on measuredZeroSnap = %+v, want Confidence=0", attTr)
	}

	invalidCases := []struct {
		name   string
		field  string
		rawVal any
		omit   bool
	}{
		{name: "quaternion empty", field: "quaternion", rawVal: []float64{}},
		{name: "quaternion short len 2", field: "quaternion", rawVal: []float64{0.5, 0.5}},
		{name: "quaternion short len 3", field: "quaternion", rawVal: []float64{1, 0, 0}},
		{name: "quaternion oversized len 5", field: "quaternion", rawVal: []float64{1, 0, 0, 0, 0}},
		{name: "quaternion null element", field: "quaternion", rawVal: []any{nil, 0.0, 0.0, 0.0}},
		{name: "quaternion null", field: "quaternion", rawVal: nil},
		{name: "quaternion omitted", field: "quaternion", omit: true},
		{name: "body_rates empty", field: "body_rates", rawVal: []float64{}},
		{name: "body_rates short len 2", field: "body_rates", rawVal: []float64{0.1, 0.2}},
		{name: "body_rates oversized len 4", field: "body_rates", rawVal: []float64{0.1, 0.2, 0.3, 0.4}},
		{name: "body_rates null element", field: "body_rates", rawVal: []any{0.0, nil, 0.0}},
		{name: "body_rates null", field: "body_rates", rawVal: nil},
		{name: "body_rates omitted", field: "body_rates", omit: true},
		{name: "ned empty", field: "ned", rawVal: []float64{}},
		{name: "ned short len 2", field: "ned", rawVal: []float64{1.0, 2.0}},
		{name: "ned oversized len 4", field: "ned", rawVal: []float64{1.0, 2.0, 3.0, 4.0}},
		{name: "ned null element", field: "ned", rawVal: []any{0.0, 0.0, nil}},
		{name: "ned null", field: "ned", rawVal: nil},
		{name: "ned omitted", field: "ned", omit: true},
	}

	for _, tc := range invalidCases {
		var envelope map[string]any
		if err := json.Unmarshal(validEnvelope, &envelope); err != nil {
			t.Fatalf("unmarshal validEnvelope map: %v", err)
		}
		payloadMap, ok := envelope["payload"].(map[string]any)
		if !ok {
			t.Fatalf("envelope[payload] = %T, want map[string]any", envelope["payload"])
		}
		if tc.omit {
			delete(payloadMap, tc.field)
		} else {
			payloadMap[tc.field] = tc.rawVal
		}
		tamperedEnvelope, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("marshal tampered envelope (%s): %v", tc.name, err)
		}
		if _, err := dec.Decode(tamperedEnvelope); err == nil {
			t.Fatalf("dec.Decode(%s) succeeded, want error", tc.name)
		}

		// Also verify direct UnmarshalJSON leaves a pre-populated struct untouched on failure.
		tamperedPayload, err := json.Marshal(payloadMap)
		if err != nil {
			t.Fatalf("marshal tampered payload (%s): %v", tc.name, err)
		}
		preserved := *valid
		if err := json.Unmarshal(tamperedPayload, &preserved); err == nil {
			t.Fatalf("json.Unmarshal(%s) succeeded, want error", tc.name)
		}
		if !reflect.DeepEqual(&preserved, valid) {
			t.Fatalf("failed json.Unmarshal(%s) mutated target struct: got %+v, want %+v", tc.name, preserved, *valid)
		}
	}

	if err := json.Unmarshal([]byte("null"), &VehicleSnapshot{}); err == nil {
		t.Fatal("json.Unmarshal(null) succeeded, want error")
	}

	// Invalidated domain (!HasHeading with HeadingObservedUnixNs > 0) MUST carry the invalidation
	// timestamp so graph.ReplaceBySource supersedes a prior measured heading instead of rejecting epochZero!
	t1 := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	t2 := t1.Add(250 * time.Millisecond)
	measuredSnap := *valid
	measuredSnap.PoseMetadata = true
	measuredSnap.HasGlobalPos = true
	measuredSnap.GlobalPosObservedUnixNs = t1.UnixNano()
	measuredSnap.HasHeading = true
	measuredSnap.HeadingDeg = 274.5
	measuredSnap.HeadingObservedUnixNs = t1.UnixNano()
	measuredSnap.ObservedAt = t1

	invalidatedSnap := measuredSnap
	invalidatedSnap.HasHeading = false
	invalidatedSnap.HeadingDeg = 0
	invalidatedSnap.HeadingObservedUnixNs = t2.UnixNano()
	invalidatedSnap.GlobalPosObservedUnixNs = t2.UnixNano()
	invalidatedSnap.ObservedAt = t2

	rep1 := graph.ReplaceBySource(nil, measuredSnap.Triples())
	rep2 := graph.ReplaceBySource(rep1.Triples, invalidatedSnap.Triples())
	if len(rep2.Stale) != 0 {
		t.Fatalf("ReplaceBySource rejected invalidated heading as stale: %+v", rep2.Stale)
	}
	for _, tr := range rep2.Triples {
		if tr.Predicate == PredicateKinematicsHeadingDeg {
			if tr.Confidence != 0.0 || tr.Context != "" || tr.Object != 0.0 || !tr.Timestamp.Equal(t2) {
				t.Fatalf("ReplaceBySource invalidated heading triple = %+v, want Object=0 Confidence=0 Timestamp=%v", tr, t2)
			}
		}
	}
}
