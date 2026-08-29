package runtime

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
)

const (
	foregroundCatalogQuietPeriod = 500 * time.Millisecond
	foregroundGatePollInterval   = 25 * time.Millisecond
)

// foregroundActivityGate lets bounded background work yield between items
// while Gallery and search requests are active. Long maintenance reads can opt
// into cancellation so a newly arriving foreground request does not wait for
// their catalog scan to finish.
type foregroundActivityGate struct {
	active           atomic.Int64
	lastActivityNano atomic.Int64
	quietPeriod      time.Duration
	mu               sync.Mutex
	backgroundSeq    uint64
	backgroundCancel map[uint64]context.CancelFunc
}

func (g *foregroundActivityGate) begin() func() {
	if g == nil {
		return func() {}
	}
	g.lastActivityNano.Store(time.Now().UnixNano())
	g.active.Add(1)
	g.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(g.backgroundCancel))
	for id, cancel := range g.backgroundCancel {
		cancels = append(cancels, cancel)
		delete(g.backgroundCancel, id)
	}
	g.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			g.lastActivityNano.Store(time.Now().UnixNano())
			g.active.Add(-1)
		})
	}
}

// beginCancelableBackground waits for a foreground-quiet window and returns a
// child context that is canceled when new foreground work arrives. The caller
// must invoke the returned release function.
func (g *foregroundActivityGate) beginCancelableBackground(ctx context.Context) (context.Context, func(), error) {
	if g == nil {
		return ctx, func() {}, nil
	}
	for {
		if err := g.waitUntilIdle(ctx); err != nil {
			return nil, nil, err
		}

		g.mu.Lock()
		quietPeriod := g.quietPeriod
		if quietPeriod <= 0 {
			quietPeriod = foregroundCatalogQuietPeriod
		}
		lastActivity := g.lastActivityNano.Load()
		quietFor := time.Since(time.Unix(0, lastActivity))
		if g.active.Load() > 0 || (lastActivity != 0 && quietFor < quietPeriod) {
			g.mu.Unlock()
			continue
		}
		backgroundCtx, cancel := context.WithCancel(ctx)
		g.backgroundSeq++
		id := g.backgroundSeq
		if g.backgroundCancel == nil {
			g.backgroundCancel = make(map[uint64]context.CancelFunc)
		}
		g.backgroundCancel[id] = cancel
		g.mu.Unlock()

		var once sync.Once
		return backgroundCtx, func() {
			once.Do(func() {
				g.mu.Lock()
				delete(g.backgroundCancel, id)
				g.mu.Unlock()
				cancel()
			})
		}, nil
	}
}

func (g *foregroundActivityGate) waitUntilIdle(ctx context.Context) error {
	if g == nil {
		return nil
	}
	quietPeriod := g.quietPeriod
	if quietPeriod <= 0 {
		quietPeriod = foregroundCatalogQuietPeriod
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastActivity := g.lastActivityNano.Load()
		quietFor := time.Since(time.Unix(0, lastActivity))
		if g.active.Load() <= 0 && (lastActivity == 0 || quietFor >= quietPeriod) {
			return nil
		}
		wait := foregroundGatePollInterval
		if g.active.Load() <= 0 && quietFor >= 0 {
			if remaining := quietPeriod - quietFor; remaining > 0 && remaining < wait {
				wait = remaining
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// foregroundMediaBody keeps the foreground window open for the complete media
// transfer, not only until the upstream response headers arrive. Media API
// handlers close every response body, and EOF is also treated as completion so
// callers that fully consume a body without an explicit close cannot pin the
// foreground gate.
type foregroundMediaBody struct {
	io.ReadCloser
	finish func()
	once   sync.Once
}

func (b *foregroundMediaBody) Read(buffer []byte) (int, error) {
	read, err := b.ReadCloser.Read(buffer)
	if err != nil {
		b.complete()
	}
	return read, err
}

func (b *foregroundMediaBody) Close() error {
	err := b.ReadCloser.Close()
	b.complete()
	return err
}

func (b *foregroundMediaBody) complete() {
	if b == nil {
		return
	}
	b.once.Do(func() {
		if b.finish != nil {
			b.finish()
		}
	})
}

func (a *AgentRuntime) foregroundMediaResponse(response *catalog.UpstreamMediaResponse, err error, finish func()) (*catalog.UpstreamMediaResponse, error) {
	if err != nil || response == nil || response.Body == nil {
		finish()
		return response, err
	}
	response.Body = &foregroundMediaBody{ReadCloser: response.Body, finish: finish}
	return response, nil
}

// runForegroundCancelableBackgroundAssignment admits long semantic work only
// in a foreground-quiet window. Gallery/search/media activity cancels the
// child context; semantic mutations are transactional or resumable, and the
// work-state repair is marked dirty so the next quiet scheduler pass observes
// any progress committed before cancellation.
func (a *AgentRuntime) runForegroundCancelableBackgroundAssignment(ctx context.Context, run func(context.Context) bool) bool {
	if a == nil || run == nil {
		return false
	}
	backgroundCtx, release, err := a.foregroundCatalog.beginCancelableBackground(ctx)
	if err != nil {
		return false
	}
	defer release()
	completed := run(backgroundCtx)
	if backgroundCtx.Err() != nil && ctx.Err() == nil {
		a.schedulerWorkStateMarkDirty()
	}
	return completed
}
