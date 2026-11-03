package t3

import (
	"context"
	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
	"strings"
	"time"
)

const historyInterval = 30 * time.Second
const historyBudget = 750 * time.Millisecond

// Full-history aggregates are optional telemetry, never liveness authority.
// Sequential scans avoid fetching payload-bearing pages in thread-index order.
// No indexes, pragmas or tables are written in T3's database.
const v2HistoryQuery = `SELECT thread_id,SUM(messages),SUM(tools) FROM (
 SELECT thread_id,COUNT(*) messages,0 tools FROM orchestration_v2_projection_messages NOT INDEXED
 WHERE role IN ('user','assistant') GROUP BY thread_id
 UNION ALL
 SELECT thread_id,0 messages,COUNT(*) tools FROM orchestration_v2_projection_nodes NOT INDEXED
 WHERE kind='tool_call' GROUP BY thread_id
) GROUP BY thread_id`

type historyCounts struct{ messages, tools int }

func (c *Collector) historyCounters(ctx context.Context, db projectionReader, threads []sessions.Summary) {
	now := c.now()
	if c.historyAttempt.IsZero() || now.Sub(c.historyAttempt) >= historyInterval {
		c.historyAttempt = now
		readCtx, cancel := context.WithTimeout(ctx, historyBudget)
		values, err := readHistory(readCtx, db)
		cancel()
		if err == nil {
			c.history = values
			c.historyAt = now
			c.historyError = ""
		} else {
			switch {
			case transientRead(err):
				c.historyError = "T3 history counters temporarily unavailable"
				if sqliteBusy(err) {
					c.historyError += "; database busy"
				} else {
					c.historyError += "; read deadline exceeded"
				}
			default:
				// Required schema is validated by the metadata statement. A
				// later optional scan failure cannot invalidate current liveness.
				c.historyError = "T3 history counters unavailable; refresh failed"
			}
		}
	}
	for i := range threads {
		counts := c.history[strings.TrimPrefix(threads[i].ID, c.prefix())]
		threads[i].MessageCount, threads[i].ToolCallCount = counts.messages, counts.tools
		threads[i].T3CountsError = c.historyError
		if !c.historyAt.IsZero() {
			at := c.historyAt
			threads[i].T3CountsAt = &at
		}
	}
}

func readHistory(ctx context.Context, db projectionReader) (map[string]historyCounts, error) {
	rows, err := db.QueryContext(ctx, v2HistoryQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string]historyCounts{}
	for rows.Next() {
		var id string
		var counts historyCounts
		if err := rows.Scan(&id, &counts.messages, &counts.tools); err != nil {
			return nil, err
		}
		values[id] = counts
	}
	return values, rows.Err()
}
