//go:build !darwin || !cgo

package macresources

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestUnsupportedNativeCollectorFailsConfigurationImmediately(t *testing.T) {
	handler := New(nil)
	err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{
		ID: AppID, Generation: 1, Config: json.RawMessage(`{}`),
	}})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ReplaceInstances error = %v, want ErrUnsupported", err)
	}
	if !pluginsdk.IsPermanentConfiguration(err) {
		t.Fatalf("ReplaceInstances error = %v, want permanent configuration classification", err)
	}
	if handler.worker != nil {
		t.Fatalf("unsupported collector launched worker %#v", handler.worker)
	}
}
