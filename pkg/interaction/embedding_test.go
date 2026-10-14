package interaction_test

import (
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/interaction"
)

func TestEmbeddingSpaceSetIsCanonicalAndRequiredAsAWhole(t *testing.T) {
	set, err := interaction.NewEmbeddingSpaceSet([]string{
		"18:embedding-space-v14:beta",
		"18:embedding-space-v15:alpha",
		"18:embedding-space-v14:beta",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Identities) != 2 ||
		set.Identities[0] != "18:embedding-space-v14:beta" ||
		set.Identities[1] != "18:embedding-space-v15:alpha" ||
		set.Digest == "" {
		t.Fatalf("canonical embedding spaces = %+v", set)
	}
	session := interaction.Session{
		ID:              interaction.DerivedID("session", "embedding-spaces"),
		RecordedAt:      time.Unix(1700000000, 0).UTC(),
		Operation:       interaction.OperationRetrieval,
		EmbeddingSpaces: set,
	}
	if err := session.Validate(); err != nil {
		t.Fatal(err)
	}
	tampered := session
	tampered.EmbeddingSpaces.Digest = interaction.Digest("different")
	if err := tampered.Validate(); err == nil {
		t.Fatal("noncanonical embedding space digest was accepted")
	}
	ambiguous := session
	ambiguous.EmbeddingSpaceID = "legacy-space"
	if err := ambiguous.Validate(); err == nil {
		t.Fatal("mixed legacy and canonical embedding pins were accepted")
	}
}
