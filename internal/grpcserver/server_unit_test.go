package grpcserver

import "testing"

func TestClampLimit(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		def       int
		max       int
		want      int
	}{
		{name: "negative uses default", requested: -1, def: 50, max: 200, want: 50},
		{name: "zero uses default", requested: 0, def: 50, max: 200, want: 50},
		{name: "within range passes through", requested: 75, def: 50, max: 200, want: 75},
		{name: "at max passes through", requested: 200, def: 50, max: 200, want: 200},
		{name: "over max clamps to max", requested: 5000, def: 50, max: 200, want: 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clampLimit(tt.requested, tt.def, tt.max)
			if got != tt.want {
				t.Errorf("clampLimit(%d, %d, %d) = %d, want %d", tt.requested, tt.def, tt.max, got, tt.want)
			}
		})
	}
}
