//go:build !darwin || !cgo

package calendar

import "context"

func newNativeEventStore() (managedEventStore, error) { return nil, ErrUnsupported }

type nativeURLOpener struct{}

func (nativeURLOpener) Open(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrUnsupported
}
