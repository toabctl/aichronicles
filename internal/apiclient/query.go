package apiclient

import (
	"net/url"
	"strconv"
)

// qparams is a tiny builder over url.Values that elides zero/empty
// values and folds the "did anything get set?" check into a single
// URL(path) call. Every apiclient method that constructs a query
// string used the same `q := url.Values{}; if x > 0 { q.Set(...) };
// path := "/v1/foo"; if enc := q.Encode(); enc != "" { path += "?" +
// enc }` boilerplate before this helper.
//
// Caller convention:
//
//	q := qparams{}
//	q.SetInt("limit", req.Limit)
//	q.SetString("kind", req.Kind)
//	return c.do(ctx, "GET", q.URL("/v1/foo"), nil, &out)
type qparams struct {
	v url.Values
}

// SetInt sets name=n when n > 0, otherwise no-ops. Matches the
// "0 = use default" convention of the wire endpoints.
func (q *qparams) SetInt(name string, n int) {
	if n > 0 {
		q.ensure().Set(name, strconv.Itoa(n))
	}
}

// SetInt64 mirrors SetInt for int64 (since_ms / cursor values).
// Zero/negative values are skipped — callers wanting "explicit 0"
// should use the underlying url.Values directly.
func (q *qparams) SetInt64(name string, n int64) {
	if n > 0 {
		q.ensure().Set(name, strconv.FormatInt(n, 10))
	}
}

// SetString sets name=v when v is non-empty.
func (q *qparams) SetString(name, v string) {
	if v != "" {
		q.ensure().Set(name, v)
	}
}

// SetBool sets name=true when b, otherwise no-ops. Used for the
// faceted filters (with_failures, no_summary).
func (q *qparams) SetBool(name string, b bool) {
	if b {
		q.ensure().Set(name, "true")
	}
}

// URL composes path with the encoded query string, or returns path
// unchanged when nothing was set. Always returns a string suitable
// for c.do(ctx, ..., result, ...).
func (q *qparams) URL(path string) string {
	if q.v == nil {
		return path
	}
	enc := q.v.Encode()
	if enc == "" {
		return path
	}
	return path + "?" + enc
}

func (q *qparams) ensure() url.Values {
	if q.v == nil {
		q.v = url.Values{}
	}
	return q.v
}
