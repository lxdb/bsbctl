package calendar

import "testing"

func TestMeetingURLAcceptsStructuredMeetAndZoomLinks(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		provider meetingProvider
	}{
		{name: "meet", raw: "https://meet.google.com/abc-defg-hij", provider: providerGoogleMeet},
		{name: "zoom root domain", raw: "https://zoom.us/j/123456789?pwd=secret", provider: providerZoom},
		{name: "zoom subdomain", raw: "https://team.zoom.us/my/team", provider: providerZoom},
		{name: "zoom government", raw: "https://agency.zoomgov.com/j/987654321", provider: providerZoom},
		{name: "explicit https port", raw: "https://zoom.us:443/j/123", provider: providerZoom},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			link, ok := meetingURL(test.raw)
			if !ok {
				t.Fatal("recognized meeting URL was rejected")
			}
			if link.Provider != test.provider || link.URL != test.raw {
				t.Fatalf("link = %#v", link)
			}
		})
	}
}

func TestMeetingURLRejectsUnsafeOrUnrecognizedLinks(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "malformed", raw: "://bad"},
		{name: "http", raw: "http://meet.google.com/abc-defg-hij"},
		{name: "credentials", raw: "https://user:password@zoom.us/j/123"},
		{name: "nonstandard port", raw: "https://zoom.us:444/j/123"},
		{name: "meet lookalike", raw: "https://meet.google.com.evil.example/abc"},
		{name: "zoom lookalike", raw: "https://zoom.us.evil.example/j/123"},
		{name: "bare meet root", raw: "https://meet.google.com/"},
		{name: "bare zoom root", raw: "https://zoom.us"},
		{name: "other provider", raw: "https://teams.microsoft.com/l/meetup-join/123"},
		{name: "custom scheme", raw: "zoommtg://zoom.us/join?confno=123"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if link, ok := meetingURL(test.raw); ok {
				t.Fatalf("unsafe URL was accepted: %#v", link)
			}
		})
	}
}
