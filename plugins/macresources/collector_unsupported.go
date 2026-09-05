//go:build !darwin || !cgo

package macresources

import (
	"context"
)

type unsupportedCollector struct{}

func NewNativeCollector() Collector { return unsupportedCollector{} }

func (unsupportedCollector) Availability() error { return ErrUnsupported }

func (unsupportedCollector) Sample(ctx context.Context) (RawSample, error) {
	if err := ctx.Err(); err != nil {
		return RawSample{}, err
	}
	return RawSample{}, ErrUnsupported
}
