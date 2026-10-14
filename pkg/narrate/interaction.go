// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"github.com/phrocker/shoal-oss/pkg/interaction"
)

// AssertedReason narrates the caller-asserted reason recorded on an
// interaction session. Its code and source are only what the caller said, so
// they are quoted and attributed, and the sentence says Shoal did not verify
// them. Free-form detail is never retained, only its digest, which is
// referenced rather than shown. A session with no asserted reason renders
// nothing.
func (r *Renderer) AssertedReason(session interaction.Session, opts Options) ([]Sentence, error) {
	b := r.begin("interaction:"+opaqueID([]byte(session.ID)), opts)
	reason := session.CallerAssertedReason
	if reason.IsZero() {
		return b.finish()
	}
	by := firstNonEmpty(string(session.Actor.ActorID), string(session.Actor.SubjectID))
	refs := principalRef(string(session.Actor.ActorID), string(session.Actor.SubjectID))
	if reason.DetailDigest != "" {
		refs = append(refs, Ref{Kind: "detail_digest", ID: reason.DetailDigest})
	}
	detail := Selector("none")
	switch {
	case reason.Source != "":
		detail = "source"
	case reason.DetailDigest != "":
		detail = "digest"
	}
	b.add(RoleReason, "interaction.asserted_reason", Args{
		"by":     b.principal(string(session.Actor.ActorID), string(session.Actor.SubjectID)),
		"code":   b.quote(AttributedToCaller, by, reason.Code),
		"source": b.quote(AttributedToCaller, by, reason.Source),
		"detail": detail,
	}, refs...)
	return b.finish()
}
