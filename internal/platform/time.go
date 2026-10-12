package platform

import "time"

// timeNow is a small indirection so callers can be overridden in tests.
var timeNow = func() time.Time { return time.Now() }

// Now returns the current time from the platform clock. Prefer it over
// time.Now() in code that wants a single, overridable time source.
func Now() time.Time { return timeNow() }
