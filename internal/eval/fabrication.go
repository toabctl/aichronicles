// Package eval holds aichronicles' offline evaluation primitives: the
// metrics and small statistical helpers used to measure whether the
// system's extraction, grounding, and (later) retrieval actually behave
// as intended. It is a leaf package — pure stdlib, no imports from the
// rest of the tree — so any package (or its tests) can depend on it
// without violating the dependency-direction invariants depcheck
// enforces.
//
// The first metric is the fabrication rate: the fraction of atoms an LLM
// pipeline emitted (skill triggers, evidence session ids, fact
// subjects/quotes, …) that are NOT attributable to the source the
// pipeline was grounded in. aichronicles' whole correctness bet
// (CLAUDE.md §7) is that its grounding filters drive this to zero on
// stored output; this package makes that measurable and gate-able.
package eval

import "strings"

// Grounder reports whether a single emitted atom is attributable to the
// source. It is the pluggable predicate at the heart of FabricationRate:
// callers supply the semantics that match the artifact — substring
// containment for a trigger against evidence quotes, exact membership
// for an evidence session id against the set of real sessions, and so
// on.
type Grounder func(atom string) bool

// SubstringGrounder returns a Grounder that treats an atom as grounded
// when it appears, case-insensitively, as a substring of any corpus
// entry. This mirrors prompts.FilterGroundedTriggers (a trigger must be
// contained in an evidence quote) and the facts quote check (a quote
// must be contained in the session substrate). Whitespace around the
// atom is trimmed before matching; an empty/whitespace-only atom is
// never grounded.
func SubstringGrounder(corpus []string) Grounder {
	lowered := make([]string, 0, len(corpus))
	for _, c := range corpus {
		lowered = append(lowered, strings.ToLower(c))
	}
	return func(atom string) bool {
		needle := strings.ToLower(strings.TrimSpace(atom))
		if needle == "" {
			return false
		}
		for _, c := range lowered {
			if strings.Contains(c, needle) {
				return true
			}
		}
		return false
	}
}

// MembershipGrounder returns a Grounder that treats an atom as grounded
// only when it exactly (after trimming) equals a member of allowed.
// This mirrors prompts.FilterEvidenceBySessionAllowList and
// GroundInductionEvidence: a hallucinated but well-formed UUID is
// rejected because it isn't in the set of sessions that actually exist.
func MembershipGrounder(allowed []string) Grounder {
	set := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		if a = strings.TrimSpace(a); a != "" {
			set[a] = struct{}{}
		}
	}
	return func(atom string) bool {
		_, ok := set[strings.TrimSpace(atom)]
		return ok
	}
}

// FabricationReport is the result of scoring a set of atoms: how many
// were emitted, how many were not attributable to the source, and the
// list of the offending (ungrounded) atoms for debugging.
type FabricationReport struct {
	// Total is the number of atoms scored.
	Total int
	// Ungrounded is the subset of atoms the Grounder rejected, in input
	// order. A zero-length (nil) slice means everything was grounded.
	Ungrounded []string
}

// Rate is the fraction of atoms that were ungrounded, in [0,1]. An empty
// atom set scores 0 — nothing emitted means nothing fabricated.
func (r FabricationReport) Rate() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(len(r.Ungrounded)) / float64(r.Total)
}

// Clean reports whether no atom was fabricated. This is the gate
// predicate: aichronicles' shipped pipelines must be Clean on their
// post-grounding output.
func (r FabricationReport) Clean() bool { return len(r.Ungrounded) == 0 }

// Fabrication scores atoms against grounded, returning how many failed
// attribution. Empty/whitespace-only atoms are ignored entirely (they
// are not emitted content); nil atoms yield an empty, Clean report.
func Fabrication(atoms []string, grounded Grounder) FabricationReport {
	var rep FabricationReport
	for _, a := range atoms {
		if strings.TrimSpace(a) == "" {
			continue
		}
		rep.Total++
		if !grounded(a) {
			rep.Ungrounded = append(rep.Ungrounded, a)
		}
	}
	return rep
}
