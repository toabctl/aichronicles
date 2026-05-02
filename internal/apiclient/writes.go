package apiclient

import (
	"context"
	"net/http"

	"github.com/toabctl/aichronicles/pkg/api"
)

// SaveLLMOutput writes one llm_outputs row, idempotent on
// (kind, prompt_hash). Returns the server's id and whether a
// new row was inserted.
func (c *Client) SaveLLMOutput(ctx context.Context, req api.SaveLLMOutputRequest) (api.SaveLLMOutputResponse, error) {
	var out api.SaveLLMOutputResponse
	if err := c.do(ctx, http.MethodPost, "/v1/llm-outputs", req, &out); err != nil {
		return api.SaveLLMOutputResponse{}, err
	}
	return out, nil
}

// SaveEpisodes replaces every episode for the named session,
// atomically. An empty Episodes slice clears the session's
// episodes.
func (c *Client) SaveEpisodes(ctx context.Context, req api.SaveEpisodesRequest) (api.SaveEpisodesResponse, error) {
	var out api.SaveEpisodesResponse
	if err := c.do(ctx, http.MethodPost, "/v1/episodes", req, &out); err != nil {
		return api.SaveEpisodesResponse{}, err
	}
	return out, nil
}

// SaveSemanticFact upserts one fact.
func (c *Client) SaveSemanticFact(ctx context.Context, req api.SaveSemanticFactRequest) (api.SaveSemanticFactResponse, error) {
	var out api.SaveSemanticFactResponse
	if err := c.do(ctx, http.MethodPost, "/v1/facts", req, &out); err != nil {
		return api.SaveSemanticFactResponse{}, err
	}
	return out, nil
}

// SaveSessionOutcome writes one session_outcomes row. Returns
// nil on 204; ErrNotFound when the session does not exist (the
// FK constraint maps to a 400 with "Session does not exist" but
// the apiclient still surfaces it as an HTTPError).
func (c *Client) SaveSessionOutcome(ctx context.Context, req api.SaveSessionOutcomeRequest) error {
	return c.do(ctx, http.MethodPost, "/v1/session-outcomes", req, nil)
}

// SaveSessionLinks replaces every outgoing link from
// req.FromSessionID atomically. Empty Links clears the set.
func (c *Client) SaveSessionLinks(ctx context.Context, req api.SaveSessionLinksRequest) error {
	return c.do(ctx, http.MethodPost, "/v1/session-links", req, nil)
}
