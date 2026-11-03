package agent

import (
	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
	"sync"
	"time"
)

const sessionInterval = 5 * time.Second

// HTTP requests serve completed snapshots. One refresh may run at a time,
// with a full interval after completion, so extra clients cannot queue reads.
// Refreshes are demand-driven; no idle timer or perpetual worker is needed.
type sessionCache struct {
	mu        sync.Mutex
	poll      func() sessions.Snapshot
	now       func() time.Time
	last      sessions.Snapshot
	completed time.Time
	running   bool
}

func (c *sessionCache) snapshot() sessions.Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if !c.running && (c.completed.IsZero() || now.Sub(c.completed) >= sessionInterval) {
		c.running = true
		go func() {
			snap := c.poll()
			c.mu.Lock()
			defer c.mu.Unlock()
			c.last = snap
			c.completed = c.now()
			c.running = false
		}()
	}
	snap := c.last
	expired := false
	snap.Sources = append([]sessions.SourceHealth(nil), snap.Sources...)
	for i := range snap.Sources {
		source := &snap.Sources[i]
		if source.Source == "t3" && source.State != "unavailable" && (source.LastSuccessAt.IsZero() || now.Sub(source.LastSuccessAt) > 5*time.Minute) {
			source.State = "unavailable"
			source.Error = "T3 cached source expired; collection pending"
			expired = true
		}
	}
	if expired {
		snap.Sessions = append([]sessions.Summary(nil), snap.Sessions...)
		snap.ActiveSessions = 0
		for i := range snap.Sessions {
			row := &snap.Sessions[i]
			if row.Source == "t3" {
				row.Active = false
				row.Status = "unavailable"
				row.T3Status = nil
			}
			if row.Active {
				snap.ActiveSessions++
			}
		}
	}
	return snap
}
