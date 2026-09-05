//go:build darwin && cgo

package macresources

/*
#include "collector_darwin.h"
*/
import "C"

import (
	"context"
	"errors"
	"time"
)

type nativeCollector struct{}

func NewNativeCollector() Collector { return nativeCollector{} }

func (nativeCollector) Availability() error { return nil }

func (nativeCollector) Sample(ctx context.Context) (RawSample, error) {
	if err := ctx.Err(); err != nil {
		return RawSample{}, err
	}
	var total, idle, received, sent, networkSet C.uint64_t
	var memory C.double
	if result := C.bsbctl_cpu_counters(&total, &idle); result != 0 {
		return RawSample{}, errors.New("read Mach CPU counters")
	}
	if result := C.bsbctl_memory_percent(&memory); result != 0 {
		return RawSample{}, errors.New("read Mach memory counters")
	}
	if result := C.bsbctl_network_counters(&received, &sent, &networkSet); result != 0 {
		return RawSample{}, errors.New("read network counters")
	}
	if err := ctx.Err(); err != nil {
		return RawSample{}, err
	}
	return RawSample{
		CPUTotal: uint64(total), CPUIdle: uint64(idle), MemoryPercent: float64(memory),
		RXBytes: uint64(received), TXBytes: uint64(sent), NetworkSet: uint64(networkSet), CollectedAt: time.Now().UTC(),
	}, nil
}
