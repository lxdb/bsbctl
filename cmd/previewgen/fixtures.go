//go:build preview

package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"strings"
)

// These 72x16 framebuffers were reviewed after rendering the hard-coded
// preview scenes. Public artifacts are regenerated only from these fixtures;
// the global framebuffer of a live device is never a publication input.

//go:embed fixtures/calendar-front.gif
var calendarFixtureGIF []byte

//go:embed fixtures/calendar-front.gif.sha256
var calendarFixtureSHA256 string

//go:embed fixtures/codex-front.gif
var codexFixtureGIF []byte

//go:embed fixtures/codex-front.gif.sha256
var codexFixtureSHA256 string

//go:embed fixtures/codex-quota-front.gif
var codexQuotaFixtureGIF []byte

//go:embed fixtures/codex-quota-front.gif.sha256
var codexQuotaFixtureSHA256 string

//go:embed fixtures/github-notifications-front.gif
var githubNotificationsFixtureGIF []byte

//go:embed fixtures/github-notifications-front.gif.sha256
var githubNotificationsFixtureSHA256 string

//go:embed fixtures/mac-resources-front.gif
var macResourcesFixtureGIF []byte

//go:embed fixtures/slack-front.gif
var slackFixtureGIF []byte

//go:embed fixtures/slack-front.gif.sha256
var slackFixtureSHA256 string

type mockFixture struct {
	name   string
	format string
	data   []byte
	sha256 string
}

var reviewedMockFixtures = []mockFixture{
	{name: "calendar-front.gif", format: "gif", data: calendarFixtureGIF, sha256: strings.TrimSpace(calendarFixtureSHA256)},
	{name: "codex-front.gif", format: "gif", data: codexFixtureGIF, sha256: strings.TrimSpace(codexFixtureSHA256)},
	{name: "codex-quota-front.gif", format: "gif", data: codexQuotaFixtureGIF, sha256: strings.TrimSpace(codexQuotaFixtureSHA256)},
	{name: "github-notifications-front.gif", format: "gif", data: githubNotificationsFixtureGIF, sha256: strings.TrimSpace(githubNotificationsFixtureSHA256)},
	{name: "mac-resources-front.gif", format: "gif", data: macResourcesFixtureGIF, sha256: "645f7a1ba20f007f10e46bf42f8f2fb7e9307f374b3f80f99580daba49ba0f91"},
	{name: "slack-front.gif", format: "gif", data: slackFixtureGIF, sha256: strings.TrimSpace(slackFixtureSHA256)},
}

func generateFixtureArtifacts() (map[string][]byte, error) {
	return generateFixtureArtifactsFrom(reviewedMockFixtures)
}

func generateFixtureArtifactsFrom(fixtures []mockFixture) (map[string][]byte, error) {
	artifacts := make(map[string][]byte, len(fixtures))
	for _, fixture := range fixtures {
		digest := sha256.Sum256(fixture.data)
		if hex.EncodeToString(digest[:]) != fixture.sha256 {
			return nil, fmt.Errorf("reviewed mock fixture %q has changed", fixture.name)
		}
		var (
			content []byte
			err     error
		)
		switch fixture.format {
		case "gif":
			var frames []timedFrame
			frames, err = decodeMockGIF(fixture.data)
			if err == nil {
				content, err = encodeGIF(frames)
			}
		default:
			err = errors.New("unsupported reviewed mock fixture format")
		}
		if err != nil {
			return nil, fmt.Errorf("generate %s from reviewed mock fixture: %w", fixture.name, err)
		}
		artifacts[fixture.name] = content
	}
	return artifacts, nil
}

func mergeCapturedFixtures(captured []mockFixture) ([]mockFixture, error) {
	if len(captured) != len(capturedPreviewArtifactNames) {
		return nil, errors.New("capture must produce every device-rendered preview fixture")
	}
	replacements := make(map[string]mockFixture, len(captured))
	for _, fixture := range captured {
		if !isCapturedPreviewArtifact(fixture.name) {
			return nil, fmt.Errorf("capture produced unexpected fixture %q", fixture.name)
		}
		if _, exists := replacements[fixture.name]; exists {
			return nil, fmt.Errorf("capture produced duplicate fixture %q", fixture.name)
		}
		replacements[fixture.name] = fixture
	}
	result := append([]mockFixture(nil), reviewedMockFixtures...)
	for index := range result {
		if replacement, exists := replacements[result[index].name]; exists {
			result[index] = replacement
			delete(replacements, result[index].name)
		}
	}
	if len(replacements) != 0 {
		return nil, errors.New("capture fixture set is incomplete")
	}
	return result, nil
}

func decodeMockGIF(data []byte) ([]timedFrame, error) {
	animation, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("decode reviewed mock GIF")
	}
	if animation.Config.Width != frontWidth || animation.Config.Height != frontHeight || animation.LoopCount != 0 || len(animation.Image) == 0 || len(animation.Image) != len(animation.Delay) {
		return nil, errors.New("reviewed mock GIF metadata is invalid")
	}
	canvas := image.NewRGBA(image.Rect(0, 0, frontWidth, frontHeight))
	frames := make([]timedFrame, 0, len(animation.Image))
	for index, frame := range animation.Image {
		if animation.Delay[index] <= 0 || len(animation.Disposal) > index && animation.Disposal[index] != gif.DisposalNone {
			return nil, errors.New("reviewed mock GIF timing is invalid")
		}
		draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Src)
		copy := image.NewRGBA(canvas.Bounds())
		draw.Draw(copy, copy.Bounds(), canvas, canvas.Bounds().Min, draw.Src)
		frames = append(frames, timedFrame{Image: copy, Delay: animation.Delay[index]})
	}
	return frames, nil
}
