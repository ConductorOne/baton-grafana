package grafana

import "testing"

func TestNextPageToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pVars   *PaginationVars
		pageLen uint64
		want    string
	}{
		{
			name:    "nil pagination vars",
			pVars:   nil,
			pageLen: 50,
			want:    "",
		},
		{
			name:    "unset size does not advance on empty page",
			pVars:   &PaginationVars{Page: 1, Size: 0},
			pageLen: 0,
			want:    "",
		},
		{
			name:    "full page advances",
			pVars:   &PaginationVars{Page: 1, Size: 50},
			pageLen: 50,
			want:    "2",
		},
		{
			name:    "partial page stops",
			pVars:   &PaginationVars{Page: 1, Size: 50},
			pageLen: 10,
			want:    "",
		},
		{
			name:    "zero-based org page advances from 0",
			pVars:   &PaginationVars{Page: 0, Size: 50},
			pageLen: 50,
			want:    "1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := nextPageToken(tt.pVars, tt.pageLen); got != tt.want {
				t.Fatalf("nextPageToken() = %q, want %q", got, tt.want)
			}
		})
	}
}
