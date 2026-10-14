// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const testIssuerFamily = "https://issuer.example#"

func testFamily() []string {
	return []string{"oidc:", "oidcid:", "entra:"}
}

// TestIdentitySchemeShape (#526): the zero scheme configures no rule; a
// configured one names a namespace in force inside its family. The stamp is
// Digest only for an identity in the namespace in force.
func TestIdentitySchemeShape(t *testing.T) {
	stable := IdentityScheme{
		Digest: auth.DigestBytes("scheme", []byte("stable")),
		Prefix: "oidcid:" + testIssuerFamily + "0123456789abcdef#",
		Family: testFamily(),
	}
	subDerived := IdentityScheme{Prefix: "oidc:" + testIssuerFamily, Family: testFamily()}
	for name, scheme := range map[string]IdentityScheme{
		"none": {}, "stable": stable, "sub-derived": subDerived,
	} {
		if err := scheme.validate(); err != nil {
			t.Fatalf("%s was refused: %v", name, err)
		}
	}
	for name, scheme := range map[string]IdentityScheme{
		"a digest without a prefix":   {Digest: stable.Digest},
		"a family without a prefix":   {Family: testFamily()},
		"a prefix outside its family": {Prefix: "mcp:", Family: testFamily()},
		"an empty namespace":          {Prefix: stable.Prefix, Family: []string{""}},
	} {
		if err := scheme.validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for identity, want := range map[shoal.ID]auth.Digest{
		shoal.ID(stable.Prefix + "oid-1"):                       stable.Digest,
		"oidcid:" + testIssuerFamily + "fedcba9876543210#oid-1": {},
		"oidc:" + testIssuerFamily + "sub-1":                    {},
		"entra:oid-1":                                           {},
	} {
		if got := stable.of(identity); got != want {
			t.Errorf("%s is stamped %x, want %x", identity, got, want)
		}
		if got := subDerived.of(identity); got != (auth.Digest{}) {
			t.Errorf("under the sub-derived scheme %s is stamped %x", identity, got)
		}
	}
}

// TestIdentitySchemeOnlyTheNamespaceInForceIsComparable: within the family,
// every namespace but the one in force is foreign — under a stable scheme
// and under the default one alike, with no list of previous schemes, and
// whatever issuer minted it — and identities outside the family
// (development, service, executor and MCP principals) are never refused.
func TestIdentitySchemeOnlyTheNamespaceInForceIsComparable(t *testing.T) {
	oid := IdentityScheme{
		Digest: auth.DigestBytes("scheme", []byte("oid")),
		Prefix: "oidcid:" + testIssuerFamily + "0123456789abcdef#",
		Family: testFamily(),
	}
	uid := IdentityScheme{
		Digest: auth.DigestBytes("scheme", []byte("uid")),
		Prefix: "oidcid:" + testIssuerFamily + "fedcba9876543210#",
		Family: testFamily(),
	}
	subDerived := IdentityScheme{Prefix: "oidc:" + testIssuerFamily, Family: testFamily()}
	outside := []shoal.ID{
		"alice", "dev-principal", "mcp:client-7", "service:indexer",
		"oidcexec:https://issuer.example#runner-1", "oidcexec:bob", "gateway",
		"shoal-explore-web-oidc",
	}
	for name, probe := range map[string]struct {
		scheme  IdentityScheme
		foreign shoal.ID
		want    string
	}{
		"sub-derived identity under a stable scheme": {oid, "oidc:" + testIssuerFamily + "bob", "oidc:" + testIssuerFamily},
		"another stable path under a stable scheme":  {oid, shoal.ID(uid.Prefix + "bob"), "oidcid:" + testIssuerFamily},
		"entra under a stable scheme":                {oid, "entra:bob", "entra:"},
		"stable identity under the default scheme":   {subDerived, shoal.ID(oid.Prefix + "bob"), "oidcid:" + testIssuerFamily},
		"entra under the default scheme":             {subDerived, "entra:bob", "entra:"},
		"another issuer's sub under the default scheme": {subDerived,
			"oidc:https://old-issuer.example#bob", "oidc:https://old-issuer.example#"},
		"another issuer's stable identity under a stable scheme": {oid,
			"oidcid:https://old-issuer.example#0123456789abcdef#bob",
			"oidcid:https://old-issuer.example#"},
		"another issuer's sub under a stable scheme": {oid,
			"oidc:https://sts.windows.net/tenant/#bob", "oidc:https://sts.windows.net/tenant/#"},
	} {
		t.Run(name, func(t *testing.T) {
			current := shoal.ID(probe.scheme.Prefix + "carol")
			clean := append(append([]shoal.ID(nil), outside...), current)
			if got := probe.scheme.foreignNamespace(clean); got != "" {
				t.Fatalf("an identity outside the family, or in force, is foreign: %q", got)
			}
			if got := probe.scheme.foreignNamespace(
				append(clean, probe.foreign)); got != probe.want {
				t.Fatalf("foreign namespace = %q, want %q", got, probe.want)
			}
		})
	}
	if got := (IdentityScheme{}).foreignNamespace(
		[]shoal.ID{"oidc:" + testIssuerFamily + "bob", "entra:bob"}); got != "" {
		t.Fatalf("a host with no scheme refuses %q", got)
	}
}

// TestIdentitySchemeFlatNamespace (#546): a non-default subject claim's
// namespace oidc:<iss>#<tag># lies inside the sub-derived oidc:<iss># as a
// string. Under a Flat scheme an identity with a '#' after Prefix is in a
// nested namespace and foreign, named without the segment after Prefix;
// under the claim's scheme a sub identity is foreign as before. A Flat flag
// without a prefix is refused.
func TestIdentitySchemeFlatNamespace(t *testing.T) {
	sub := IdentityScheme{Prefix: "oidc:" + testIssuerFamily, Family: testFamily(), Flat: true}
	claim := IdentityScheme{
		Digest: auth.DigestBytes("scheme", []byte("oid")),
		Prefix: "oidc:" + testIssuerFamily + "0123456789abcdef#",
		Family: testFamily(),
	}
	for name, scheme := range map[string]IdentityScheme{"sub": sub, "claim": claim} {
		if err := scheme.validate(); err != nil {
			t.Fatalf("%s was refused: %v", name, err)
		}
	}
	if err := (IdentityScheme{Flat: true}).validate(); err == nil {
		t.Fatal("a flat scheme without a prefix was accepted")
	}
	plain := shoal.ID(sub.Prefix + "alice")
	for _, nested := range []shoal.ID{
		shoal.ID(claim.Prefix + "bob"),
		shoal.ID(sub.Prefix + "fedcba9876543210#bob"),
		shoal.ID(sub.Prefix + "bob#"),
		shoal.ID(sub.Prefix + "#"),
	} {
		if got := sub.foreignNamespace([]shoal.ID{plain, nested}); got != sub.Prefix+"<nested>#" {
			t.Errorf("under sub, %s is foreign as %q", nested, got)
		}
		if strings.Contains(sub.foreignNamespace([]shoal.ID{nested}), "bob") {
			t.Errorf("the refusal for %s names the value", nested)
		}
	}
	if got := sub.foreignNamespace([]shoal.ID{plain, "oidc:https://old.example#bob"}); got !=
		"oidc:https://old.example#" {
		t.Fatalf("under sub, another issuer is foreign as %q", got)
	}
	if got := claim.foreignNamespace([]shoal.ID{shoal.ID(claim.Prefix + "a#b"), plain}); got !=
		"oidc:"+testIssuerFamily {
		t.Fatalf("under the claim, a sub identity is foreign as %q", got)
	}
	if got := claim.foreignNamespace([]shoal.ID{shoal.ID(claim.Prefix + "a#b")}); got != "" {
		t.Fatalf("a claim value containing '#' is foreign under its own scheme: %q", got)
	}
	if claim.of(shoal.ID(claim.Prefix+"a#b")) != claim.Digest || claim.of(plain) != (auth.Digest{}) {
		t.Fatal("the claim scheme's stamp does not follow its namespace")
	}
	flatStamped := sub
	flatStamped.Digest = claim.Digest
	if flatStamped.of(shoal.ID(claim.Prefix+"bob")) != (auth.Digest{}) ||
		flatStamped.of(plain) != claim.Digest {
		t.Fatal("a flat scheme's stamp does not follow its namespace")
	}
}
