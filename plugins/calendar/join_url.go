package calendar

import (
	"net/url"
	"strings"
)

type meetingProvider string

const (
	providerGoogleMeet meetingProvider = "google_meet"
	providerZoom       meetingProvider = "zoom"
)

type meetingLink struct {
	Provider meetingProvider
	URL      string
}

func meetingURL(raw string) (meetingLink, bool) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return meetingLink{}, false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil {
		return meetingLink{}, false
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return meetingLink{}, false
	}
	if parsed.Path == "" || parsed.Path == "/" {
		return meetingLink{}, false
	}
	host := strings.ToLower(parsed.Hostname())
	switch {
	case host == "meet.google.com":
		return meetingLink{Provider: providerGoogleMeet, URL: raw}, true
	case host == "zoom.us" || strings.HasSuffix(host, ".zoom.us") ||
		host == "zoomgov.com" || strings.HasSuffix(host, ".zoomgov.com"):
		return meetingLink{Provider: providerZoom, URL: raw}, true
	default:
		return meetingLink{}, false
	}
}
