package app

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	defaultRetentionMaintenanceInterval = time.Minute
	defaultRetentionOperationTimeout    = 5 * time.Second
)

// retentionTicker and retentionClock isolate the scheduler from wall time so
// lifecycle tests can drive a maintenance cycle without sleeping.
type retentionTicker interface {
	C() <-chan time.Time
	Stop()
}

type retentionClock interface {
	Now() time.Time
	NewTicker(time.Duration) retentionTicker
}

type wallRetentionClock struct{}

func (wallRetentionClock) Now() time.Time { return time.Now().UTC() }
func (wallRetentionClock) NewTicker(interval time.Duration) retentionTicker {
	return wallRetentionTicker{Ticker: time.NewTicker(interval)}
}

type wallRetentionTicker struct{ *time.Ticker }

func (t wallRetentionTicker) C() <-chan time.Time { return t.Ticker.C }

// retentionMaintenanceOptions are construction seams. Production leaves them
// zero-valued; tests inject a clock and inspect bounded real storage work.
type retentionMaintenanceOptions struct {
	Clock                retentionClock
	DeleteExpiredContent func(context.Context, time.Duration, time.Time) (int64, error)
	OperationTimeout     time.Duration
	OnCycleComplete      func(error)
}

// retentionMaintenance deletes only encrypted content through the repository.
// It holds no raw content and stores only a failure bit, so readiness cannot
// accidentally retain or expose a storage error that might contain data.
type retentionMaintenance struct {
	retention        time.Duration
	clock            retentionClock
	ticker           retentionTicker
	deleteExpired    func(context.Context, time.Duration, time.Time) (int64, error)
	operationTimeout time.Duration
	onCycleComplete  func(error)
	cancel           context.CancelFunc
	done             chan struct{}

	mu     sync.RWMutex
	failed bool
}

func newRetentionMaintenance(retention, interval time.Duration, deleteExpired func(context.Context, time.Duration, time.Time) (int64, error), options retentionMaintenanceOptions) *retentionMaintenance {
	if retention <= 0 {
		return nil
	}
	clock := options.Clock
	if clock == nil {
		clock = wallRetentionClock{}
	}
	if interval == 0 {
		interval = defaultRetentionMaintenanceInterval
	}
	operationTimeout := options.OperationTimeout
	if operationTimeout <= 0 {
		operationTimeout = defaultRetentionOperationTimeout
	}
	if options.DeleteExpiredContent != nil {
		deleteExpired = options.DeleteExpiredContent
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &retentionMaintenance{
		retention: retention, clock: clock, ticker: clock.NewTicker(interval), deleteExpired: deleteExpired,
		operationTimeout: operationTimeout, onCycleComplete: options.OnCycleComplete, cancel: cancel, done: make(chan struct{}),
	}
	go m.run(ctx)
	return m
}

func (m *retentionMaintenance) run(ctx context.Context) {
	defer close(m.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.ticker.C():
			m.runOnce(ctx)
		}
	}
}

func (m *retentionMaintenance) runOnce(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, m.operationTimeout)
	_, err := m.deleteExpired(ctx, m.retention, m.clock.Now())
	cancel()
	m.mu.Lock()
	m.failed = err != nil
	m.mu.Unlock()
	if m.onCycleComplete != nil {
		m.onCycleComplete(err)
	}
}

func (m *retentionMaintenance) Ready() error {
	m.mu.RLock()
	failed := m.failed
	m.mu.RUnlock()
	if failed {
		return errors.New("retention maintenance is unavailable")
	}
	return nil
}

// Close cancels any bounded operation and waits for the loop to exit before a
// caller can close SQLite. It is idempotent through App.Close's closeOnce.
func (m *retentionMaintenance) Close() {
	m.cancel()
	m.ticker.Stop()
	<-m.done
}
