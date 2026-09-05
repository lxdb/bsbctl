package attention

import (
	"bytes"
	"io"
	"testing"
)

type countingLedgerReader struct {
	reader io.Reader
	read   int
}

func (r *countingLedgerReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

func TestLedgerReadDoesNotAllocateBeyondBound(t *testing.T) {
	const bound = 1024
	reader := &countingLedgerReader{reader: bytes.NewReader(bytes.Repeat([]byte("x"), 1024*1024))}
	_, _, err := consumeJSONLines(reader, bound, func([]byte) error { return nil })
	if err == nil {
		t.Fatal("oversize archive accepted")
	}
	if reader.read > bound+1 {
		t.Fatalf("read %d bytes from oversized unterminated ledger with limit %d", reader.read, bound)
	}
}
