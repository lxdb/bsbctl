package calendar

import "errors"

const (
	nativeAccessNotDetermined = iota
	nativeAccessRestricted
	nativeAccessDenied
	nativeAccessWriteOnly
	nativeAccessFull
)

var ErrUnsupported = errors.New("Calendar integration requires macOS with cgo enabled")

type managedEventStore interface {
	eventStore
	Changes() <-chan struct{}
	Close() error
}

func accessStatusFromNative(status int) accessStatus {
	switch status {
	case nativeAccessNotDetermined:
		return accessNotDetermined
	case nativeAccessRestricted:
		return accessRestricted
	case nativeAccessDenied:
		return accessDenied
	case nativeAccessWriteOnly:
		return accessWriteOnly
	case nativeAccessFull:
		return accessFull
	default:
		return accessUnknown
	}
}
