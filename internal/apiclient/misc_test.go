package apiclient

import (
	"context"
	"errors"
	"testing"
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
