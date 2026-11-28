// Package control implements non-retrying MAVLink v2 discrete actuation (#76 -> #77),
// lease-bound 10-30 Hz velocity teleoperation (#84), 1 Hz GCS liveness (#0),
// instant hardware preemption, and same-origin HTTP control handlers (`OQ2 Option A`).
package control

import (
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/c360studio/semengine/pkg/lifecycle"
	semtypes "github.com/c360studio/semengine/pkg/types"
	"github.com/c360studio/semengine/vocabulary"
)

const (
	// PackName identifies the discrete UXS command vocabulary pack registered with SemEngine.
	PackName = "bbops-uxs-control"

	// EntityTypeCommand is the canonical <domain>.<type> key for discrete UXS command entities.
	EntityTypeCommand = "uxs.command"

	// WorkflowName is the semengine/pkg/lifecycle workflow name for discrete #76 commands.
	WorkflowName = "uxs-command"

	// CommandEntityIDPattern matches 6-token discrete command entity IDs in ENTITY_STATES.
	CommandEntityIDPattern = "*.*.robotics.uxs.command.*"

	// PredicateCommandPhase records the discrete command phase (`pending`, `confirmed`, `rejected`, `commit_unknown`, `preempted`).
	PredicateCommandPhase = "uxs.command.phase"
	// PredicateCommandTarget records the target UXS vehicle entity ID (`<org>.<platform>.robotics.uxs.vehicle.<sysid>`).
	PredicateCommandTarget = "uxs.command.target"
	// PredicateCommandSysID records the target MAVLink system ID (`1..=254`).
	PredicateCommandSysID = "uxs.command.sysid"
	// PredicateCommandCmdID records the standard MIT common.xml MAV_CMD numeric identifier.
	PredicateCommandCmdID = "uxs.command.cmdid"
	// PredicateCommandAction records the canonical bbops discrete command action slug.
	PredicateCommandAction = "uxs.command.action"
	// PredicateCommandPeer records the normalized UDP peer endpoint (`ip:port`) to which #76 COMMAND_LONG was dispatched.
	PredicateCommandPeer = "uxs.command.peer"
	// PredicateCommandDomain records the admitted UXS vehicle domain at dispatch (`quadcopter`, `rover`, `surface-boat`).
	PredicateCommandDomain = "uxs.command.domain"
	// PredicateCommandLinkTier records the admitted UXS LinkTier at dispatch (`sitl-local`, `direct-bench`, `mesh-supervisory`).
	PredicateCommandLinkTier = "uxs.command.linktier"
	// PredicateCommandAckResult records the #77 COMMAND_ACK MAV_RESULT (`-1` while unacknowledged or ambiguous).
	PredicateCommandAckResult = "uxs.command.ackresult"
	// PredicateCommandAckAccounted records (`1` vs `0`) whether an unattributed/ambiguous or idle #77 COMMAND_ACK slot was accounted for.
	PredicateCommandAckAccounted = "uxs.command.ackaccounted"
	// PredicateCommandAckPoolMask records the cumulative bitmask of #77 COMMAND_ACK MAV_RESULT codes observed on `(sysid, cmd_id)`.
	PredicateCommandAckPoolMask = "uxs.command.ackpoolmask"
	// PredicateCommandDetail records operator-facing status or refusal detail text.
	PredicateCommandDetail = "uxs.command.detail"
	// PredicateCommandIssuedMs records the UTC Unix-millisecond dispatch timestamp.
	PredicateCommandIssuedMs = "uxs.command.issuedms"
	// PredicateCommandUpdatedMs records the UTC Unix-millisecond latest transition timestamp.
	PredicateCommandUpdatedMs = "uxs.command.updatedms"

	// Discrete ADR-111 command lifecycle phases.
	PhasePending       = "pending"
	PhaseConfirmed     = "confirmed"
	PhaseRejected      = "rejected"
	PhaseCommitUnknown = "commit_unknown"
	PhasePreempted     = "preempted"

	// UnacknowledgedResult is stamped into uxs.command.ackresult once a #76 COMMAND_LONG is
	// persisted/sent without an unambiguously attributed #77 COMMAND_ACK so TransitionWith emits
	// an explicit triple delta when an unambiguous MAV_RESULT_ACCEPTED (0) arrives.
	UnacknowledgedResult = -1

	// UnsentAckResult marks a CommandEntity that was aborted or rejected prior to transmitting
	// a #76 COMMAND_LONG datagram on UDP so restart recovery does not count an unreceived #77 ACK.
	UnsentAckResult = -2

	// CommitUnknownDetail is the canonical indeterminate status message surfaced when #77 COMMAND_ACK
	// does not arrive within the command timeout (`ADR-111 Decision 3`, zero blind retries).
	CommitUnknownDetail = "Status unconfirmed — awaiting vehicle heartbeat"
)

// Canonical discrete operator actions accepted by POST /control/command.
const (
	ActionArm                   = "arm"
	ActionDisarm                = "disarm"
	ActionHoldPosition          = "hold-position"
	ActionModeTeleop            = "mode-teleop"
	ActionModeAutonomous        = "mode-autonomous"
	ActionSITLCompanionBlackout = "sitl-companion-blackout"
	ActionSITLCRSFOverride      = "sitl-crsf-override"
	ActionSITLCrossCurrent      = "sitl-cross-current"
)

// Canonical #84 coordinate frame selectors accepted by POST /control/teleop.
const (
	FrameLocalNED = "local-ned"
	FrameBodyNED  = "body-ned"
)

// CommandEntity projects a discrete #76 COMMAND_LONG execution into SemEngine ENTITY_STATES
// via semengine/pkg/lifecycle.Manager revision-fenced CAS transitions (`OQ2 Option A`).
type CommandEntity struct {
	ID           string `lifecycle:"id"`
	PhaseState   string `lifecycle:"phase,predicate=uxs.command.phase"`
	Target       string `lifecycle:"predicate=uxs.command.target"`
	SysID        int    `lifecycle:"predicate=uxs.command.sysid"`
	CmdID        int    `lifecycle:"predicate=uxs.command.cmdid"`
	Action       string `lifecycle:"predicate=uxs.command.action"`
	Peer         string `json:"peer,omitempty" lifecycle:"predicate=uxs.command.peer"`
	Domain       string `json:"domain,omitempty" lifecycle:"predicate=uxs.command.domain"`
	LinkTier     string `json:"link_tier,omitempty" lifecycle:"predicate=uxs.command.linktier"`
	AckResult    int    `lifecycle:"predicate=uxs.command.ackresult"`
	AckAccounted int    `json:"ack_accounted,omitempty" lifecycle:"predicate=uxs.command.ackaccounted,operator_writable"`
	AckPoolMask  int    `json:"ack_pool_mask,omitempty" lifecycle:"predicate=uxs.command.ackpoolmask,operator_writable"`
	Detail       string `lifecycle:"predicate=uxs.command.detail"`
	IssuedMs     int64  `lifecycle:"predicate=uxs.command.issuedms"`
	UpdatedMs    int64  `lifecycle:"predicate=uxs.command.updatedms"`
}

var _ lifecycle.Participant = (*CommandEntity)(nil)

var commandTransitions = lifecycle.Transitions{
	PhasePending:       {PhaseConfirmed, PhaseRejected, PhaseCommitUnknown, PhasePreempted},
	PhaseCommitUnknown: {PhaseConfirmed, PhaseRejected, PhasePreempted},
	PhaseConfirmed:     {},
	PhaseRejected:      {},
	PhasePreempted:     {},
}

// EntityID implements lifecycle.Participant.
func (c *CommandEntity) EntityID() string { return c.ID }

// Workflow implements lifecycle.Participant.
func (c *CommandEntity) Workflow() string { return WorkflowName }

// Phase implements lifecycle.Participant.
func (c *CommandEntity) Phase() string { return c.PhaseState }

// IsTerminal implements lifecycle.Participant.
func (c *CommandEntity) IsTerminal() bool { return commandTransitions.IsTerminal(c.PhaseState) }

// ParentEntityID implements lifecycle.Participant.
func (c *CommandEntity) ParentEntityID() string { return "" }

// CommandEntityID constructs and validates the canonical 6-part entity ID
// `<org>.<platform>.robotics.uxs.command.<instance>` for a discrete command.
func CommandEntityID(org, platform, instance string) (string, error) {
	org = strings.TrimSpace(org)
	platform = strings.TrimSpace(platform)
	instance = strings.TrimSpace(instance)
	if org == "" || strings.Contains(org, ".") ||
		platform == "" || strings.Contains(platform, ".") ||
		instance == "" || strings.Contains(instance, ".") {
		return "", fmt.Errorf("control: org, platform, and instance must be non-empty single segments")
	}
	entityID := org + "." + platform + ".robotics.uxs.command." + instance
	if len(entityID) > semtypes.MaxEntityIDBytes {
		return "", fmt.Errorf("control: command entity ID length %d exceeds %d bytes", len(entityID), semtypes.MaxEntityIDBytes)
	}
	if _, err := semtypes.ParseEntityID(entityID); err != nil {
		return "", fmt.Errorf("control: invalid command entity ID %q: %w", entityID, err)
	}
	return entityID, nil
}

// ControlPackSpec returns the declarative vocabulary pack for discrete UXS commands.
func ControlPackSpec() vocabulary.PackSpec {
	return vocabulary.PackSpec{
		Name:    PackName,
		Version: "0.1.0",
		Predicates: []vocabulary.PredicateSpec{
			{
				Name:        PredicateCommandPhase,
				Description: "Discrete UXS command execution phase (pending, confirmed, rejected, commit_unknown, preempted)",
				DataType:    "string",
			},
			{
				Name:        PredicateCommandTarget,
				Description: "Target UXS vehicle entity ID in ENTITY_STATES",
				DataType:    "string",
			},
			{
				Name:        PredicateCommandSysID,
				Description: "Target MAVLink v2 vehicle system ID (1..254)",
				DataType:    "int",
			},
			{
				Name:        PredicateCommandCmdID,
				Description: "Standard MIT common.xml MAV_CMD numeric identifier",
				DataType:    "int",
			},
			{
				Name:        PredicateCommandAction,
				Description: "Canonical bbops discrete command action identifier",
				DataType:    "string",
			},
			{
				Name:        PredicateCommandPeer,
				Description: "Normalized UDP peer endpoint (ip:port) to which COMMAND_LONG was dispatched",
				DataType:    "string",
			},
			{
				Name:        PredicateCommandDomain,
				Description: "Admitted UXS vehicle domain at command dispatch",
				DataType:    "string",
			},
			{
				Name:        PredicateCommandLinkTier,
				Description: "Admitted UXS LinkTier at command dispatch",
				DataType:    "string",
			},
			{
				Name:        PredicateCommandAckResult,
				Description: "Unambiguously attributed inbound #77 COMMAND_ACK MAV_RESULT code (-1 while unacknowledged or ambiguous)",
				DataType:    "int",
			},
			{
				Name:        PredicateCommandAckAccounted,
				Description: "Owner-neutral slot accounting marker (1 when a #77 COMMAND_ACK slot for the command pool was consumed)",
				DataType:    "int",
			},
			{
				Name:        PredicateCommandAckPoolMask,
				Description: "Owner-neutral bitmask for the command pool: bits 0..26 record observed #77 COMMAND_ACK MAV_RESULT codes; bits 27..30 carry reserved continuity/accounting flags (bit 27 target-continuity displaced, bit 28 wrap-held receipt credit, bit 29 settled wrap credit, bit 30 v2 format marker)",
				DataType:    "int",
			},
			{
				Name:        PredicateCommandDetail,
				Description: "Operator-facing discrete command status or refusal detail",
				DataType:    "string",
			},
			{
				Name:        PredicateCommandIssuedMs,
				Description: "Discrete command dispatch UTC timestamp in milliseconds",
				DataType:    "int",
				Units:       "ms",
			},
			{
				Name:        PredicateCommandUpdatedMs,
				Description: "Discrete command latest phase transition UTC timestamp in milliseconds",
				DataType:    "int",
				Units:       "ms",
			},
		},
		EntityTypes: []vocabulary.EntityTypeSpec{
			{
				DomainType:  EntityTypeCommand,
				Description: "Non-retrying discrete MAVLink v2 COMMAND_LONG execution tracked in ENTITY_STATES",
			},
		},
	}
}

var registerVocabMu sync.Mutex

// RegisterVocab registers the bbops-uxs-control vocabulary pack idempotently.
func RegisterVocab() error {
	registerVocabMu.Lock()
	defer registerVocabMu.Unlock()
	if vocabulary.RequireDeclaredPredicate(PredicateCommandPhase) == nil {
		return nil
	}
	pack := ControlPackSpec()
	if err := pack.Validate(); err != nil {
		return fmt.Errorf("control: validate vocabulary pack %q: %w", pack.Name, err)
	}
	if err := vocabulary.RegisterPack(pack); err != nil {
		return fmt.Errorf("control: register vocabulary pack %q: %w", pack.Name, err)
	}
	return nil
}

// Workflow returns the semengine/pkg/lifecycle Workflow definition for uxs.command entities.
func Workflow() lifecycle.Workflow {
	return lifecycle.Workflow{
		Name:            WorkflowName,
		EntityIDPattern: CommandEntityIDPattern,
		Phases:          []string{PhasePending, PhaseConfirmed, PhaseRejected, PhaseCommitUnknown, PhasePreempted},
		PhasePredicate:  PredicateCommandPhase,
		Schema:          reflect.TypeOf(CommandEntity{}),
		Transitions:     commandTransitions,
		OperatorWritablePredicates: []string{
			PredicateCommandAckAccounted,
			PredicateCommandAckPoolMask,
		},
	}
}

// RegisterWorkflow registers the bbops-uxs-control vocabulary pack and attaches
// the uxs-command Workflow to mgr.
func RegisterWorkflow(mgr *lifecycle.Manager) error {
	if mgr == nil {
		return fmt.Errorf("control: nil lifecycle manager")
	}
	if err := RegisterVocab(); err != nil {
		return err
	}
	return mgr.Register(Workflow())
}
