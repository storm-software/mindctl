package headroom

import "errors"

// ErrUnavailable is the stable classification for every managed Headroom
// failure. Callers must not fall back to an uncompressed provider request.
var ErrUnavailable = errors.New("headroom unavailable")
