package calendar

import "testing"

func TestAccessStatusFromNativeFailsClosedForUnknownValues(t *testing.T) {
	tests := []struct {
		native int
		want   accessStatus
	}{
		{native: nativeAccessNotDetermined, want: accessNotDetermined},
		{native: nativeAccessRestricted, want: accessRestricted},
		{native: nativeAccessDenied, want: accessDenied},
		{native: nativeAccessWriteOnly, want: accessWriteOnly},
		{native: nativeAccessFull, want: accessFull},
		{native: -1, want: accessUnknown},
		{native: 99, want: accessUnknown},
	}
	for _, test := range tests {
		if got := accessStatusFromNative(test.native); got != test.want {
			t.Fatalf("native status %d = %q, want %q", test.native, got, test.want)
		}
	}
}
