package main

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

)

func TestAppSetupCommandRoutesAndHelpAdvertisesSecureInput(t *testing.T) {
	called := false
	setup := func(_ context.Context, args []string, _ io.Reader, _, _ io.Writer) error {
		called = true
		if !reflect.DeepEqual(args, []string{"github", "--file", "setup.json", "--token-stdin"}) {
			t.Fatalf("setup args = %#v", args)
		}
		return nil
	}
	if err := runAppWithSetup(t.Context(), []string{"setup", "github", "--file", "setup.json", "--token-stdin"}, strings.NewReader(""), io.Discard, io.Discard, setup); err != nil || !called {
		t.Fatalf("route error = %v, called = %t", err, called)
	}
	var help bytes.Buffer
	if err := usage(&help); err != nil || !strings.Contains(help.String(), "bsbctl app setup <app-id> --file CONFIG [--token-stdin]") {
		t.Fatalf("help error = %v\n%s", err, help.String())
	}
}
