package api

// Problem is the RFC 7807 problem+json shape every error response
// from aichronicles-api uses. Title is short and human-stable,
// suitable for logging; Detail is variable per occurrence and may
// echo validation messages or transient context. Status mirrors
// the HTTP status code.
//
// Wire content-type: application/problem+json.
type Problem struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}
