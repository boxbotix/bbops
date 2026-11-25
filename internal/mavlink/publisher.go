package mavlink

import (
	"context"
	"slices"
	"sync"
)

// BatchPublishFunc publishes a batch of serialized BaseMessage payloads onto a JetStream subject.
type BatchPublishFunc func(ctx context.Context, subject string, msgs [][]byte) error

// UrgentBuildFunc builds serialized SnapshotBatchItems for a sorted slice of urgent sysids.
type UrgentBuildFunc func(sysids []uint8) ([]SnapshotBatchItem, error)

// PeriodicBuildFunc builds serialized SnapshotBatchItems at flush time for sessions
// whose monotonic revision exceeds confirmedRevs[sysid].
type PeriodicBuildFunc func(confirmedRevs map[uint8]uint64) ([]SnapshotBatchItem, error)

// Publisher decouples UDP socket reads and periodic SKG ticks from JetStream acknowledgements,
// draining urgent dirty sysids strictly ahead of coalesced periodic notifications and building
// periodic batches at flush time filtered by lastConfirmedRev[sysid] (`OQ3 Option A`).
type Publisher struct {
	subject       string
	publishBatch  BatchPublishFunc
	buildUrgent   UrgentBuildFunc
	buildPeriodic PeriodicBuildFunc
	onError       func()

	wakeCh chan struct{}

	mu              sync.Mutex
	urgentSysIDs    map[uint8]struct{}
	periodicPending bool
	flushing        bool

	lastConfirmedRev map[uint8]uint64
}

// NewPublisher creates a non-blocking build-at-flush SKG batch publisher for subject.
func NewPublisher(
	subject string,
	publishBatch BatchPublishFunc,
	buildUrgent UrgentBuildFunc,
	buildPeriodic PeriodicBuildFunc,
	onError func(),
) *Publisher {
	if onError == nil {
		onError = func() {}
	}
	return &Publisher{
		subject:          subject,
		publishBatch:     publishBatch,
		buildUrgent:      buildUrgent,
		buildPeriodic:    buildPeriodic,
		onError:          onError,
		wakeCh:           make(chan struct{}, 1),
		urgentSysIDs:     make(map[uint8]struct{}),
		lastConfirmedRev: make(map[uint8]uint64),
	}
}

// NotifyPeriodic marks a coalesced Lane 2 periodic SKG flush pending without blocking
// or marshaling snapshots on the ticker goroutine.
func (p *Publisher) NotifyPeriodic() {
	p.mu.Lock()
	p.periodicPending = true
	p.mu.Unlock()

	select {
	case p.wakeCh <- struct{}{}:
	default:
	}
}

// NotifyUrgent marks sysid dirty for an immediate out-of-band Lane 3 SKG flush
// without blocking the UDP read loop.
func (p *Publisher) NotifyUrgent(sysid uint8) {
	p.mu.Lock()
	p.urgentSysIDs[sysid] = struct{}{}
	p.mu.Unlock()

	select {
	case p.wakeCh <- struct{}{}:
	default:
	}
}

func (p *Publisher) hasPending() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.urgentSysIDs) > 0 || p.periodicPending || p.flushing
}

// Run drains urgent dirty sysids strictly ahead of coalesced periodic flushes until ctx is canceled.
// Must be invoked by a single worker goroutine.
func (p *Publisher) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		urgent, periodic := p.takeWork()
		if len(urgent) > 0 {
			p.flushUrgent(ctx, urgent)
			p.finishWork()
			continue
		}
		if periodic {
			p.flushPeriodic(ctx)
			p.finishWork()
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-p.wakeCh:
		}
	}
}

func (p *Publisher) takeWork() (urgent []uint8, periodic bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.urgentSysIDs) > 0 {
		urgent = make([]uint8, 0, len(p.urgentSysIDs))
		for sysid := range p.urgentSysIDs {
			urgent = append(urgent, sysid)
		}
		clear(p.urgentSysIDs)
		slices.Sort(urgent)
		p.flushing = true
		return urgent, false
	}
	if p.periodicPending {
		p.periodicPending = false
		p.flushing = true
		return nil, true
	}
	return nil, false
}

func (p *Publisher) finishWork() {
	p.mu.Lock()
	p.flushing = false
	p.mu.Unlock()
}

func (p *Publisher) flushUrgent(ctx context.Context, sysids []uint8) {
	if p.buildUrgent == nil || len(sysids) == 0 {
		return
	}
	items, err := p.buildUrgent(sysids)
	if err != nil {
		p.onError()
		return
	}
	p.flushItems(ctx, items)
}

func (p *Publisher) flushPeriodic(ctx context.Context) {
	if p.buildPeriodic == nil {
		return
	}
	items, err := p.buildPeriodic(p.lastConfirmedRev)
	if err != nil {
		p.onError()
		return
	}
	p.flushItems(ctx, items)
}

func (p *Publisher) flushItems(ctx context.Context, items []SnapshotBatchItem) {
	if len(items) == 0 || p.publishBatch == nil || ctx.Err() != nil {
		return
	}
	filtered := make([]SnapshotBatchItem, 0, len(items))
	msgs := make([][]byte, 0, len(items))
	for _, it := range items {
		if it.Rev <= p.lastConfirmedRev[it.SysID] {
			continue
		}
		filtered = append(filtered, it)
		msgs = append(msgs, it.Payload)
	}
	if len(msgs) == 0 {
		return
	}
	if err := p.publishBatch(ctx, p.subject, msgs); err != nil {
		if ctx.Err() == nil {
			p.onError()
		}
		return
	}
	for _, it := range filtered {
		if it.Rev > p.lastConfirmedRev[it.SysID] {
			p.lastConfirmedRev[it.SysID] = it.Rev
		}
	}
}
