package slack

import (
	"context"
	"strconv"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

// Fixed counters keep provider-controlled strings and cardinality out of logs.
var diagnosticCodes = [...]string{
	"invalid_envelope", "unsupported_envelope", "invalid_event", "unsupported_event",
	"queue_overflow", "disconnected", "auth_required", "missing_scope",
	"unproven_authorization", "throttled", "request_failed", "invalid_response",
	"checkpoint_invalid", "checkpoint_failed",
}

func (w *worker) recordDiagnostic(code string) {
	for i, allowed := range diagnosticCodes {
		if code == allowed {
			w.diagnostics[i].Add(1)
			return
		}
	}
}

// One reporter is joined by run. Host I/O never runs on the socket reader,
// reducer, or publisher. Each interval attempts one batch, including on failure;
// counts are best-effort diagnostics and are not retried after ambiguous errors.
func (w *worker) runDiagnostics() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			if w.ctx.Err() != nil {
				return
			}
			fields := make(map[string]string)
			for i, code := range diagnosticCodes {
				if count := w.diagnostics[i].Swap(0); count != 0 {
					fields[code] = strconv.FormatUint(count, 10)
				}
			}
			if len(fields) == 0 || w.host == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(w.ctx, 2*time.Second)
			_ = w.host.Log(ctx, protocol.LogNotification{Level: protocol.LogLevelWarn, Event: "slack_diagnostics", Instance: w.instance.Ref(), Fields: fields})
			cancel()
		}
	}
}
