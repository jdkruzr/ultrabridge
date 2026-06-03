// Package hlc is the Hybrid Logical Clock for op_ts (spec/hlc.md), mirroring the Kotlin Hlc.
// Millisecond-unit: the value is wall-clock ms dragged strictly forward on every event, so
// causality never inverts and legacy raw-wall_ts values (plain ms) interoperate with no flag.
package hlc

// Clock is a single replica's HLC. Not safe for concurrent use; serialize at the call site.
// last is the durable state — persist it and seed New from max(stored, wallNow) on load.
type Clock struct {
	last int64
	wall func() int64
}

// New builds a clock seeded at last, reading wall time via wall (e.g. time.Now().UnixMilli).
func New(last int64, wall func() int64) *Clock {
	return &Clock{last: last, wall: wall}
}

// Last returns the durable clock state.
func (c *Clock) Last() int64 { return c.last }

// LocalEvent stamps a locally-authored op: last = max(wallNow, last+1). Returns the new op_ts.
func (c *Clock) LocalEvent() int64 {
	c.last = max(c.wall(), c.last+1)
	return c.last
}

// ReceiveEvent absorbs a remote op's timestamp so future local events sort strictly after it:
// last = max(wallNow, last+1, remote+1). Returns the new last (for persistence).
func (c *Clock) ReceiveEvent(remote int64) int64 {
	c.last = max(c.wall(), c.last+1, remote+1)
	return c.last
}
