package headroom

import "errors"

// ErrUnavailable is the stable classification for every managed Headroom
// failure. Callers must not fall back to an uncompressed provider request.
var ErrUnavailable = errors.New("headroom unavailable")

// ErrRejected marks a compression result that failed round-trip validation.
// The service worked but its rewrite is unsafe for this content, so retrying
// fails the same way; callers forward the original request instead.
var ErrRejected = errors.New("headroom compression rejected")
