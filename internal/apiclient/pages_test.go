package apiclient

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/toabctl/aichronicles/internal/wire"
)

// fakePages serves n ints in pages, emitting a cursor whenever a page
// comes back full — the server's stop rule, including the extra empty
// page when n is a multiple of the page size.
func fakePages(n int) func(context.Context, wire.Cursor, int) ([]int, wire.Cursor, error) {
	return func(_ context.Context, cursor wire.Cursor, limit int) ([]int, wire.Cursor, error) {
		off := 0
		if cursor != "" {
			off, _ = strconv.Atoi(string(cursor))
		}
		var page []int
		for i := off; i < n && len(page) < limit; i++ {
			page = append(page, i)
		}
		var next wire.Cursor
		if len(page) == limit {
			next = wire.Cursor(strconv.Itoa(off + len(page)))
		}
		return page, next, nil
	}
}

func TestCollectPages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		n, max        int
		wantLen       int
		wantTruncated bool
	}{
		{"uncapped, spans pages", 2500, 0, 2500, false},
		{"cap below total", 2500, 1200, 1200, true},
		{"cap equals total", 1200, 1200, 1200, false},
		{"cap above total", 30, 50, 30, false},
		{"exact page multiple", 2000, 0, 2000, false},
		{"empty", 0, 10, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, truncated, err := collectPages(t.Context(), tc.max, fakePages(tc.n))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.wantLen || truncated != tc.wantTruncated {
				t.Errorf("got %d items truncated=%v, want %d/%v", len(got), truncated, tc.wantLen, tc.wantTruncated)
			}
			for i, v := range got {
				if v != i {
					t.Fatalf("item %d = %d: gap or reorder", i, v)
				}
			}
		})
	}
}

func TestCollectPages_StuckCursorIsAnError(t *testing.T) {
	t.Parallel()
	stuck := func(context.Context, wire.Cursor, int) ([]int, wire.Cursor, error) {
		return []int{1}, "same", nil
	}
	if _, _, err := collectPages(t.Context(), 0, stuck); err == nil {
		t.Fatal("expected an error for a non-advancing cursor")
	}
	boom := errors.New("boom")
	fail := func(context.Context, wire.Cursor, int) ([]int, wire.Cursor, error) { return nil, "", boom }
	if _, _, err := collectPages(t.Context(), 0, fail); !errors.Is(err, boom) {
		t.Errorf("fetch error not propagated: %v", err)
	}
}
