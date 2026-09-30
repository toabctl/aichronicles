package apiclient

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/toabctl/aichronicles/internal/wire"
)

func TestClient_LLMOutputByHash_NotFoundIsErrNotFound(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)
	_, err := c.LLMOutputByHash(context.Background(), "summary", "ghost-hash")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestClient_Summary_NotFoundIsErrNotFound(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)
	_, err := c.Summary(context.Background(), "no-such-session")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestClient_LLMOutputs_FollowsCursor walks more rows than one page
// holds against a real server and checks every row arrives once,
// newest first, and that breaking early stops fetching.
func TestClient_LLMOutputs_FollowsCursor(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)
	const n = wire.MaxPageLimit + 7
	for i := range n {
		if _, err := c.SaveLLMOutput(t.Context(), wire.SaveLLMOutputRequest{
			Kind: "induction", Model: "m", PromptHash: "h" + strconv.Itoa(i), Body: "{}", CreatedAtMs: int64(1000 + i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var got []int64
	for o, err := range c.LLMOutputs(t.Context(), "induction", "") {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, o.CreatedAtMs)
	}
	if len(got) != n {
		t.Fatalf("walked %d rows, want %d", len(got), n)
	}
	for i := 1; i < len(got); i++ {
		if got[i] >= got[i-1] {
			t.Fatalf("row %d not newest-first or duplicated: %d after %d", i, got[i], got[i-1])
		}
	}
	seen := 0
	for _, err := range c.LLMOutputs(t.Context(), "induction", "") {
		if err != nil {
			t.Fatal(err)
		}
		seen++
		if seen == 3 {
			break
		}
	}
	if seen != 3 {
		t.Errorf("early break: saw %d", seen)
	}
}
