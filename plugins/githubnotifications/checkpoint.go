package githubnotifications

import "errors"

// ErrStaleNotification rejects an effect whose frozen item version is no longer current.
var ErrStaleNotification = errors.New("notification selection is stale")
