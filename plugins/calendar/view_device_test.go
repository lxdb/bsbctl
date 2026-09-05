package calendar

import (
	"context"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	busylib "github.com/lxdb/busylib-go"
)

func TestCalendarScenePassesTheProductionDeviceGateway(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	deadline := now.Add(5 * time.Minute)
	display := &calendarRecordingDisplay{}
	gateway, err := device.NewGateway(display, assets.NewReconciler(nil))
	if err != nil {
		t.Fatal(err)
	}
	candidate := presentation.Candidate{
		PluginID: PluginID, InstanceID: AppID, Channel: ChannelUpcoming, Key: "event-test",
		Revision: 1, Generation: 1, AdmissionSequence: 1, Policy: presentation.PolicyAttention, Band: presentation.BandActionable, Impact: protocol.ImpactNormal,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: deadline,
		Scene: calendarScene(calendarCard{
			Channel: ChannelUpcoming, State: "NEXT", Title: "Planning review",
			CountdownAt: deadline,
		}),
	}

	if _, err := gateway.Render(context.Background(), &candidate); err != nil {
		t.Fatalf("render Calendar scene: %v", err)
	}
	if len(display.draws) != 1 {
		t.Fatalf("draws = %d, want 1", len(display.draws))
	}
	countdowns := 0
	for _, element := range display.draws[0].Elements {
		if countdown, ok := element.(busylib.CountdownElement); ok {
			countdowns++
			if countdown.Timestamp != "1787504700" || countdown.Direction != busylib.CountdownTimeLeft {
				t.Fatalf("translated countdown = %#v", countdown)
			}
		}
	}
	if countdowns != 2 {
		t.Fatalf("native countdown elements = %d, want 2", countdowns)
	}
}

type calendarRecordingDisplay struct{ draws []busylib.DisplayElements }

func (d *calendarRecordingDisplay) Draw(_ context.Context, request busylib.DisplayElements) error {
	d.draws = append(d.draws, request)
	return nil
}

func (*calendarRecordingDisplay) Clear(context.Context, string) error { return nil }
