package mavlink

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
)

// PeriodicQueueCap is the bounded drop-oldest channel capacity for periodic Lane 2 batches (`D3 Option A`).
const PeriodicQueueCap = 2

// BatchPublishFunc publishes a batch of serialized BaseMessage payloads onto a JetStream subject.
type BatchPublishFunc func(ctx context.Context, subject string, msgs [][]byte) error

// UrgentBuildFunc builds serialized SnapshotBatchItems for a sorted slice of urgent sysids.
type UrgentBuildFunc func(sysids []uint8) ([]SnapshotBatchItem, error)

// Publisher decouples the UDP socket read loop from JetStream acknowledgements (`D3 Option A`),
// pairing a cap-2 drop-oldest periodic batch channel with an urgent dirty-sysid set drained
// strictly ahead of periodic batches and guarded by monotonic per-sysid state revisions.
type Publisher struct {
	subject      string
	publishBatch BatchPublishFunc
	buildUrgent  UrgentBuildFunc
	onError      func()

	periodicCh chan []SnapshotBatchItem
	urgentWake chan struct{}

	urgentMu     sync.Mutex
	urgentSysIDs map[uint8]struct{}

	droppedPeriodic  atomic.Uint64
	lastPublishedRev map[uint8]uint64
}

// NewPublisher creates a bounded non-blocking SKG batch publisher for subject.
func NewPublisher(
	subject string,
	publishBatch BatchPublishFunc,
	buildUrgent UrgentBuildFunc,
	onError func(),
) *Publisher {
	if onError == nil {
		onError = func() {}
	}
	return &Publisher{
		subject:          subject,
		publishBatch:     publishBatch,
		buildUrgent:      buildUrgent,
		onError:          onError,
		periodicCh:       make(chan []SnapshotBatchItem, PeriodicQueueCap),
		urgentWake:       make(chan struct{}, 1),
		urgentSysIDs:     make(map[uint8]struct{}),
		lastPublishedRev: make(map[uint8]uint64),
	}
}

// EnqueuePeriodic pushes a periodic Lane 2 batch onto the cap-2 queue without blocking,
// dropping the oldest unconsumed periodic batch when full.
func (p *Publisher) EnqueuePeriodic(batch []SnapshotBatchItem) {
	if len(batch) == 0 {
		return
	}
	for {
		select {
		case p.periodicCh <- batch:
			return
		default:
			select {
			case <-p.periodicCh:
				p.droppedPeriodic.Add(1)
			default:
			}
		}
	}
}

// NotifyUrgent marks sysid dirty for an immediate out-of-band Lane 3 SKG flush
// without blocking the UDP read loop.
func (p *Publisher) NotifyUrgent(sysid uint8) {
	p.urgentMu.Lock()
	p.urgentSysIDs[sysid] = struct{}{}
	p.urgentMu.Unlock()

	select {
	case p.urgentWake <- struct{}{}:
	default:
	}
}

// DroppedPeriodic returns the total number of stale periodic batches evicted by drop-oldest backpressure.
func (p *Publisher) DroppedPeriodic() uint64 {
	return p.droppedPeriodic.Load()
}

// Run drains urgent dirty sysids ahead of periodic batches until ctx is canceled.
// Must be invoked by a single worker goroutine.
func (p *Publisher) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if urgent := p.takeUrgentSysIDs(); len(urgent) > 0 {
			_ = p.flushUrgent(ctx, urgent)
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-p.urgentWake:
			continue
		case batch := <-p.periodicCh:
			if ctx.Err() != nil {
				return
			}
			var flushedUrgent map[uint8]struct{}
			if urgent := p.takeUrgentSysIDs(); len(urgent) > 0 {
				flushedUrgent = p.flushUrgent(ctx, urgent)
			}
			if ctx.Err() != nil {
				return
			}
			p.flushItems(ctx, batch, flushedUrgent)
		}
	}
}

func (p *Publisher) takeUrgentSysIDs() []uint8 {
	p.urgentMu.Lock()
	if len(p.urgentSysIDs) == 0 {
		p.urgentMu.Unlock()
		return nil
	}
	out := make([]uint8, 0, len(p.urgentSysIDs))
	for sysid := range p.urgentSysIDs {
		out = append(out, sysid)
	}
	clear(p.urgentSysIDs)
	p.urgentMu.Unlock()

	// Drain any pending wake token now that urgentSysIDs was snapshotted.
	select {
	case <-p.urgentWake:
	default:
	}

	slices.Sort(out)
	return out
}

func (p *Publisher) flushUrgent(ctx context.Context, sysids []uint8) map[uint8]struct{} {
	if p.buildUrgent == nil || len(sysids) == 0 {
		return nil
	}
	items, err := p.buildUrgent(sysids)
	if err != nil {
		p.onError()
		return nil
	}
	if !p.flushItems(ctx, items, nil) {
		return nil
	}
	flushed := make(map[uint8]struct{}, len(items))
	for _, it := range items {
		flushed[it.SysID] = struct{}{}
	}
	return flushed
}

func (p *Publisher) flushItems(ctx context.Context, items []SnapshotBatchItem, skipEqualSysIDs map[uint8]struct{}) bool {
	if len(items) == 0 || p.publishBatch == nil {
		return false
	}
	filtered := make([]SnapshotBatchItem, 0, len(items))
	msgs := make([][]byte, 0, len(items))
	for _, it := range items {
		lastRev := p.lastPublishedRev[it.SysID]
		if it.Rev < lastRev {
			continue
		}
		if _, skipEq := skipEqualSysIDs[it.SysID]; skipEq && it.Rev == lastRev {
			continue
		}
		filtered = append(filtered, it)
		msgs = append(msgs, it.Payload)
	}
	if len(msgs) == 0 {
		return true
	}
	if err := p.publishBatch(ctx, p.subject, msgs); err != nil {
		if ctx.Err() == nil {
			p.onError()
		}
		return false
	}
	for _, it := range filtered {
		if it.Rev > p.lastPublishedRev[it.SysID] {
			p.lastPublishedRev[it.SysID] = it.Rev
		}
	}
	return true
}
