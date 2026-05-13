package apiclient

import "testing"

func TestQParamsEmpty(t *testing.T) {
	t.Parallel()
	var q qparams
	if got := q.URL("/v1/foo"); got != "/v1/foo" {
		t.Errorf("empty URL: %q, want /v1/foo", got)
	}
}

func TestQParamsSkipsZero(t *testing.T) {
	t.Parallel()
	var q qparams
	q.SetInt("limit", 0)
	q.SetInt64("since_ms", 0)
	q.SetString("kind", "")
	q.SetBool("with_failures", false)
	if got := q.URL("/v1/foo"); got != "/v1/foo" {
		t.Errorf("all zero: %q, want /v1/foo", got)
	}
}

func TestQParamsSetsNonZero(t *testing.T) {
	t.Parallel()
	var q qparams
	q.SetInt("limit", 50)
	q.SetInt64("since_ms", 123456)
	q.SetString("kind", "summary")
	q.SetBool("with_failures", true)
	got := q.URL("/v1/foo")
	// Order is fixed by url.Values.Encode (alphabetical by key).
	want := "/v1/foo?kind=summary&limit=50&since_ms=123456&with_failures=true"
	if got != want {
		t.Errorf("composed URL = %q, want %q", got, want)
	}
}

func TestQParamsURLEscapesValue(t *testing.T) {
	t.Parallel()
	var q qparams
	q.SetString("cwd", "/home/user/dir with space")
	got := q.URL("/v1/projects")
	want := "/v1/projects?cwd=%2Fhome%2Fuser%2Fdir+with+space"
	if got != want {
		t.Errorf("escaping: %q, want %q", got, want)
	}
}
