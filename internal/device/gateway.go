// Package device owns the single serialized BUSY Bar output boundary.
package device

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/presentation"
	busylib "github.com/lxdb/busylib-go"
)

const (
	ApplicationName    = "bsbctl"
	maxPlayedAudioCues = 256
)

// Display is the narrow busylib-go surface required by the output worker.
type Display interface {
	Draw(context.Context, busylib.DisplayElements) error
	Clear(context.Context, string) error
}

// BusyTimerDisplay is implemented by the process-wide device output owner.
// It is separate from Display so scene-only test and alternate displays stay narrow.
type BusyTimerDisplay interface {
	BusySnapshot(context.Context) (busylib.BusySnapshot, error)
	SetBusySnapshot(context.Context, busylib.BusySnapshot) error
	DeviceTime(context.Context) (busylib.TimestampInfo, error)
}

type AudioDisplay interface {
	PlayAudio(context.Context, busylib.PlayAudio) error
}

type AssetResolver interface {
	ResolveScene(string, presentation.Scene) (presentation.ResolvedScene, error)
}

type AudioAssetResolver interface {
	ResolveAudioCue(string, presentation.AudioCue) (presentation.ResolvedAudioCue, error)
}

type audioCueIdentity struct {
	pluginID   string
	instanceID string
	generation uint64
	cueID      string
	assetPath  string
}

func (i audioCueIdentity) less(other audioCueIdentity) bool {
	if i.pluginID != other.pluginID {
		return i.pluginID < other.pluginID
	}
	if i.instanceID != other.instanceID {
		return i.instanceID < other.instanceID
	}
	if i.generation != other.generation {
		return i.generation < other.generation
	}
	if i.assetPath != other.assetPath {
		return i.assetPath < other.assetPath
	}
	return i.cueID < other.cueID
}

type preparedAudioCue struct {
	request   busylib.PlayAudio
	identity  audioCueIdentity
	expiresAt time.Time
}

// AudioStatus reports redacted diagnostics for best-effort audio delivery.
// Attempts advances only when the gateway issues a distinct audio write.
type AudioStatus struct {
	Attempts      uint64 `json:"attempts"`
	LastErrorCode string `json:"last_error_code,omitempty"`
}

// Gateway serializes output, suppresses identical scenes, updates stable
// element topologies in place, and clears before structural replacements.
type Gateway struct {
	mu            sync.Mutex
	display       Display
	lastDigest    [sha256.Size]byte
	lastTopology  [sha256.Size]byte
	hasScene      bool
	lastPriority  int
	assets        AssetResolver
	busyCardID    string
	busyDigest    [sha256.Size]byte
	now           func() time.Time
	verifyDelay   func(context.Context, time.Duration) error
	playedAudio   map[audioCueIdentity]time.Time
	lastAudioErr  string
	audioAttempts uint64
}

func NewGateway(display Display, resolver AssetResolver) (*Gateway, error) {
	if display == nil {
		return nil, errors.New("device display is required")
	}
	if resolver == nil {
		return nil, errors.New("asset resolver is required")
	}
	return newGateway(display, resolver), nil
}

func newGateway(display Display, resolver AssetResolver) *Gateway {
	return &Gateway{display: display, assets: resolver, now: time.Now, verifyDelay: waitRuntimeDelay, playedAudio: make(map[audioCueIdentity]time.Time)}
}

// Status returns the gateway-owned best-effort audio diagnostics. Visual
// delivery is returned directly by Render.
func (g *Gateway) Status() AudioStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return AudioStatus{Attempts: g.audioAttempts, LastErrorCode: g.lastAudioErr}
}

// InvalidateCanvas forgets the cached HTTP scene after a physical selector
// transition or Back press. Firmware closes that canvas before the matching
// input event is handled, so the next selected scene must be drawn even when
// its digest is unchanged.
func (g *Gateway) InvalidateCanvas() {
	g.mu.Lock()
	g.resetScene()
	g.mu.Unlock()
}

// InvalidateConnection forgets physical-delivery proofs after reconnect while
// retaining ownership and structure for clearing a scene or timer that survived.
func (g *Gateway) InvalidateConnection() {
	g.mu.Lock()
	g.lastDigest = [sha256.Size]byte{}
	g.busyDigest = [sha256.Size]byte{}
	g.mu.Unlock()
}

// Render updates a structurally compatible bsbctl canvas in place. A nil
// candidate clears it; structural changes and priority drops replace it.
func (g *Gateway) Render(ctx context.Context, candidate *presentation.Candidate) (attention.DeliveryOutcome, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.display == nil {
		return attention.OutcomeDeviceUnavailable, errors.New("device display is not configured")
	}
	if candidate == nil {
		stoppedTimer, err := g.stopOwnedBusyTimer(ctx)
		if err != nil {
			return attention.OutcomeDeviceUnavailable, err
		}
		if !g.hasScene {
			if stoppedTimer {
				return attention.OutcomeCleared, nil
			}
			return attention.OutcomeUnchanged, nil
		}
		if err := g.display.Clear(ctx, ApplicationName); err != nil {
			return attention.OutcomeDeviceUnavailable, fmt.Errorf("clear bsbctl display: %w", err)
		}
		g.hasScene = false
		g.lastDigest = [sha256.Size]byte{}
		g.lastTopology = [sha256.Size]byte{}
		g.lastPriority = 0
		return attention.OutcomeCleared, nil
	}
	if err := candidate.Validate(); err != nil {
		return attention.OutcomeInvalidPresentation, fmt.Errorf("%w: validate selected candidate: %v", presentation.ErrInvalidPresentation, err)
	}
	resolved, request, audioCue, err := g.compileCandidate(*candidate)
	if err != nil {
		return renderErrorOutcome(err, attention.OutcomeInvalidPresentation), err
	}
	if candidate.BusyTimer != nil {
		outcome, err := g.renderBusyTimer(ctx, *candidate)
		if err != nil {
			return outcome, err
		}
		g.playAudioCue(ctx, audioCue)
		return outcome, nil
	}
	if _, err := g.stopOwnedBusyTimer(ctx); err != nil {
		return attention.OutcomeDeviceUnavailable, err
	}
	topology := sceneTopology(resolved)
	payload, err := json.Marshal(request)
	if err != nil {
		return attention.OutcomeInvalidPresentation, fmt.Errorf("encode selected scene: %w", err)
	}
	digest := sha256.Sum256(payload)
	if g.hasScene && digest == g.lastDigest {
		g.playAudioCue(ctx, audioCue)
		return attention.OutcomeUnchanged, nil
	}
	if g.hasScene && (topology != g.lastTopology || request.Priority < g.lastPriority) {
		if err := g.display.Clear(ctx, ApplicationName); err != nil {
			return attention.OutcomeDeviceUnavailable, fmt.Errorf("clear before replacing display scene: %w", err)
		}
		g.hasScene = false
		g.lastDigest = [sha256.Size]byte{}
		g.lastTopology = [sha256.Size]byte{}
		g.lastPriority = 0
	}
	if err := g.display.Draw(ctx, request); err != nil {
		if apiErr, ok := errors.AsType[*busylib.APIError](err); ok && apiErr.StatusCode == http.StatusConflict {
			g.resetScene()
			g.resetBusyTimer()
			return attention.OutcomeFirmwareSuppressed, nil
		}
		return attention.OutcomeDeviceUnavailable, fmt.Errorf("draw bsbctl display: %w", err)
	}
	g.hasScene = true
	g.lastDigest = digest
	g.lastTopology = topology
	g.lastPriority = request.Priority
	g.playAudioCue(ctx, audioCue)
	return attention.OutcomeDrawn, nil
}

type renderOutcomeError struct {
	outcome attention.DeliveryOutcome
	err     error
}

func (e *renderOutcomeError) Error() string { return e.err.Error() }
func (e *renderOutcomeError) Unwrap() error { return e.err }

func renderErrorOutcome(err error, fallback attention.DeliveryOutcome) attention.DeliveryOutcome {
	if value, ok := errors.AsType[*renderOutcomeError](err); ok {
		return value.outcome
	}
	return fallback
}

func (g *Gateway) compileCandidate(candidate presentation.Candidate) (presentation.ResolvedScene, busylib.DisplayElements, *preparedAudioCue, error) {
	var resolved presentation.ResolvedScene
	var request busylib.DisplayElements
	if candidate.BusyTimer == nil {
		resolved = presentation.ResolveScene(candidate.Scene)
		if g.assets != nil {
			var err error
			resolved, err = g.assets.ResolveScene(candidate.PluginID, candidate.Scene)
			if err != nil {
				return presentation.ResolvedScene{}, busylib.DisplayElements{}, nil, &renderOutcomeError{
					outcome: attention.OutcomeAssetMissing, err: fmt.Errorf("resolve selected assets: %w", err),
				}
			}
		}
		var err error
		request, err = presentation.CompileScene(ApplicationName, candidate.EffectiveDevicePriority(), resolved)
		if err != nil {
			return presentation.ResolvedScene{}, busylib.DisplayElements{}, nil, err
		}
	} else if err := presentation.CompileBusyTimer(candidate.BusyTimer.Theme); err != nil {
		return presentation.ResolvedScene{}, busylib.DisplayElements{}, nil, err
	}
	audio, err := g.compileAudioCue(candidate)
	if err != nil {
		return presentation.ResolvedScene{}, busylib.DisplayElements{}, nil, err
	}
	return resolved, request, audio, nil
}

func (g *Gateway) compileAudioCue(candidate presentation.Candidate) (*preparedAudioCue, error) {
	cue := candidate.AudioCue
	if cue == nil || !cue.ExpiresAt.After(g.now()) {
		return nil, nil
	}
	resolved := presentation.ResolveAudioCue(*cue)
	if resolver, ok := g.assets.(AudioAssetResolver); ok {
		var err error
		resolved, err = resolver.ResolveAudioCue(candidate.PluginID, *cue)
		if err != nil {
			return nil, &renderOutcomeError{
				outcome: attention.OutcomeAssetMissing, err: fmt.Errorf("resolve selected audio asset: %w", err),
			}
		}
	}
	request, err := presentation.CompileAudio(ApplicationName, resolved)
	if err != nil {
		return nil, err
	}
	identity := audioCueIdentity{
		pluginID: candidate.PluginID, instanceID: candidate.InstanceID, generation: candidate.Generation,
		cueID: cue.ID, assetPath: resolved.Path,
	}
	return &preparedAudioCue{request: request, identity: identity, expiresAt: cue.ExpiresAt}, nil
}

func (g *Gateway) playAudioCue(ctx context.Context, cue *preparedAudioCue) {
	if cue == nil {
		return
	}
	now := g.now()
	maps.DeleteFunc(g.playedAudio, func(_ audioCueIdentity, expiresAt time.Time) bool { return !expiresAt.After(now) })
	if !cue.expiresAt.After(now) {
		return
	}
	if _, played := g.playedAudio[cue.identity]; played {
		return
	}
	g.lastAudioErr = ""
	g.audioAttempts++
	g.playedAudio[cue.identity] = cue.expiresAt
	if len(g.playedAudio) > maxPlayedAudioCues {
		oldestID := cue.identity
		oldestExpiry := cue.expiresAt
		for id, expiresAt := range g.playedAudio {
			if expiresAt.Before(oldestExpiry) || (expiresAt.Equal(oldestExpiry) && id.less(oldestID)) {
				oldestID, oldestExpiry = id, expiresAt
			}
		}
		delete(g.playedAudio, oldestID)
	}
	audio, ok := g.display.(AudioDisplay)
	if !ok {
		g.lastAudioErr = "audio_unsupported"
		return
	}
	if err := audio.PlayAudio(ctx, cue.request); err != nil {
		g.lastAudioErr = "audio_play_failed"
		return
	}
}

func (g *Gateway) renderBusyTimer(ctx context.Context, candidate presentation.Candidate) (attention.DeliveryOutcome, error) {
	device, ok := g.display.(BusyTimerDisplay)
	if !ok {
		return attention.OutcomeDeviceUnavailable, errors.New("device display does not support native BUSY timers")
	}
	cardID := busyCardID(candidate.ID())
	payload, err := json.Marshal(struct {
		CardID    string    `json:"card_id"`
		Theme     string    `json:"theme"`
		ExpiresAt time.Time `json:"expires_at"`
	}{CardID: cardID, Theme: candidate.BusyTimer.Theme, ExpiresAt: candidate.ExpiresAt.UTC()})
	if err != nil {
		return attention.OutcomeInvalidPresentation, fmt.Errorf("encode selected busy timer: %w", err)
	}
	digest := sha256.Sum256(payload)
	if g.busyCardID != "" && digest == g.busyDigest {
		return attention.OutcomeUnchanged, nil
	}
	remaining := candidate.ExpiresAt.Sub(g.now())
	if remaining <= 0 {
		return attention.OutcomeInvalidPresentation, errors.New("selected busy timer has expired")
	}
	remainingMS := remaining.Milliseconds()
	if remainingMS <= 0 {
		remainingMS = 1
	}
	if err := g.display.Clear(ctx, ApplicationName); err != nil {
		return attention.OutcomeDeviceUnavailable, fmt.Errorf("clear before starting busy timer: %w", err)
	}
	g.resetScene()
	current, err := device.BusySnapshot(ctx)
	if err != nil {
		return attention.OutcomeDeviceUnavailable, fmt.Errorf("read current busy timer: %w", err)
	}
	deviceTimestamp, err := device.DeviceTime(ctx)
	if err != nil {
		return attention.OutcomeDeviceUnavailable, fmt.Errorf("read device time: %w", err)
	}
	deviceNow, err := time.Parse(time.RFC3339, deviceTimestamp.Timestamp)
	if err != nil {
		return attention.OutcomeDeviceUnavailable, fmt.Errorf("parse device time: %w", err)
	}
	timestampMS := max(current.SnapshotTimestampMS+1, deviceNow.UnixMilli())
	if timestampMS > deviceNow.Add(time.Minute).UnixMilli() {
		return attention.OutcomeDeviceUnavailable, errors.New("busy timer snapshot timestamp is too far ahead of device time")
	}
	paused := false
	settings := current.Snapshot.BusyBarSettings
	settings.Theme = candidate.BusyTimer.Theme
	request := busylib.BusySnapshot{
		SnapshotTimestampMS: timestampMS,
		Snapshot: busylib.BusySnapshotData{
			Type: busylib.BusySnapshotSimple, CardID: cardID, TimeLeftMS: &remainingMS,
			IsPaused: &paused, BusyBarSettings: settings,
		},
	}
	if err := device.SetBusySnapshot(ctx, request); err != nil {
		return attention.OutcomeDeviceUnavailable, fmt.Errorf("start native busy timer: %w", err)
	}
	// A failed readback does not undo the write. Retain its exact ownership for
	// cleanup, but do not cache delivery until verification succeeds.
	g.busyCardID = cardID
	g.busyDigest = [sha256.Size]byte{}
	if err := g.verifyBusyTimer(ctx, device, cardID, candidate.BusyTimer.Theme, remainingMS); err != nil {
		return attention.OutcomeDeviceUnavailable, err
	}
	g.busyDigest = digest
	return attention.OutcomeDrawn, nil
}

func (g *Gateway) verifyBusyTimer(ctx context.Context, device BusyTimerDisplay, cardID, theme string, requestedMS int64) error {
	for attempt := 0; attempt < 3; attempt++ {
		value, err := device.BusySnapshot(ctx)
		if err != nil {
			return fmt.Errorf("verify native busy timer: %w", err)
		}
		data := value.Snapshot
		if data.Type == busylib.BusySnapshotSimple && data.CardID == cardID && data.BusyBarSettings.Theme == theme &&
			data.IsPaused != nil && !*data.IsPaused && data.TimeLeftMS != nil && *data.TimeLeftMS > 0 && *data.TimeLeftMS <= requestedMS {
			return nil
		}
		if attempt < 2 {
			if err := g.verifyDelay(ctx, 50*time.Millisecond); err != nil {
				return fmt.Errorf("wait to verify native busy timer: %w", err)
			}
		}
	}
	return errors.New("device did not apply native busy timer snapshot")
}

func (g *Gateway) stopOwnedBusyTimer(ctx context.Context) (bool, error) {
	if g.busyCardID == "" {
		return false, nil
	}
	device, ok := g.display.(BusyTimerDisplay)
	if !ok {
		return false, errors.New("device display does not support native BUSY timers")
	}
	current, err := device.BusySnapshot(ctx)
	if err != nil {
		return false, fmt.Errorf("read busy timer before stopping: %w", err)
	}
	if current.Snapshot.CardID != g.busyCardID {
		g.resetBusyTimer()
		return false, nil
	}
	deviceTimestamp, err := device.DeviceTime(ctx)
	if err != nil {
		return false, fmt.Errorf("read device time before stopping busy timer: %w", err)
	}
	deviceNow, err := time.Parse(time.RFC3339, deviceTimestamp.Timestamp)
	if err != nil {
		return false, fmt.Errorf("parse device time: %w", err)
	}
	timestampMS := max(current.SnapshotTimestampMS+1, deviceNow.UnixMilli())
	if timestampMS > deviceNow.Add(time.Minute).UnixMilli() {
		return false, errors.New("busy timer stop timestamp is too far ahead of device time")
	}
	request := busylib.BusySnapshot{
		SnapshotTimestampMS: timestampMS,
		Snapshot:            busylib.BusySnapshotData{Type: busylib.BusySnapshotNotStarted, BusyBarSettings: current.Snapshot.BusyBarSettings},
	}
	if err := device.SetBusySnapshot(ctx, request); err != nil {
		return false, fmt.Errorf("stop native busy timer: %w", err)
	}
	g.resetBusyTimer()
	return true, nil
}

func (g *Gateway) resetScene() {
	g.hasScene = false
	g.lastDigest = [sha256.Size]byte{}
	g.lastTopology = [sha256.Size]byte{}
	g.lastPriority = 0
}

func (g *Gateway) resetBusyTimer() {
	g.busyCardID = ""
	g.busyDigest = [sha256.Size]byte{}
}

func busyCardID(identity string) string {
	value := sha256.Sum256([]byte(identity))
	value[6] = (value[6] & 0x0f) | 0x50
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

func sceneTopology(scene presentation.ResolvedScene) [sha256.Size]byte {
	type elementTopology struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Display string `json:"display"`
	}
	elements := make([]elementTopology, 0, len(scene.Elements))
	for _, element := range scene.Elements {
		elements = append(elements, elementTopology{ID: element.ID, Type: resolvedElementType(element), Display: string(element.Display)})
	}
	slices.SortFunc(elements, func(left, right elementTopology) int { return cmp.Compare(left.ID, right.ID) })
	payload, _ := json.Marshal(elements)
	return sha256.Sum256(payload)
}

func resolvedElementType(element presentation.ResolvedElement) string {
	switch {
	case element.Text != nil:
		return "text"
	case element.Image != nil:
		return "image"
	case element.Animation != nil:
		return "animation"
	case element.Rectangle != nil:
		return "rectangle"
	case element.Countdown != nil:
		return "countdown"
	default:
		return ""
	}
}
