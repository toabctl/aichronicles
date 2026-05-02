package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestClient_Import_HappyPath(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)

	env := validEnvelope(t)
	body, _ := json.Marshal(env)
	body = append(body, '\n')

	stats, err := c.Import(context.Background(), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if stats.Imported != 1 {
		t.Errorf("Imported: got %d, want 1", stats.Imported)
	}
	if stats.LinesRead != 1 {
		t.Errorf("LinesRead: got %d, want 1", stats.LinesRead)
	}
}

func TestClient_Import_DaemonDownIsErrSocketUnavailable(t *testing.T) {
	t.Parallel()
	missing := "/tmp/nonexistent-import-test.sock"
	c := NewClient(missing)
	_, err := c.Import(context.Background(), strings.NewReader(""))
	if !errors.Is(err, ErrSocketUnavailable) {
		t.Errorf("got %v, want ErrSocketUnavailable", err)
	}
}
