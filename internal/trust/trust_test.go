package trust

import (
	"errors"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
)

func TestValidatePolicy(t *testing.T) {
	good := []domain.CoordinationTrust{
		{Authority: AuthoritySignedCommitTip, Signatures: SignaturesRequired, AllowedSignersFile: "/etc/fxvcs/allowed_signers"},
		{Authority: AuthoritySignedPromotionTag, Signatures: SignaturesRequired, AllowedSignersFile: "/etc/fxvcs/allowed_signers", PromotionTagPrefix: "refs/tags/fxvcs/promote/prd/"},
		{Authority: AuthoritySignedCommitTip, Signatures: SignaturesOff},
	}
	for _, p := range good {
		if err := ValidatePolicy(p); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	bad := []domain.CoordinationTrust{
		{Authority: "branch-tip", Signatures: SignaturesOff},
		{Authority: AuthoritySignedCommitTip, Signatures: SignaturesRequired},                                                   // no signers file
		{Authority: AuthoritySignedCommitTip, Signatures: SignaturesOff, PromotionTagPrefix: "refs/tags/fxvcs/promote/prd/"},    // prefix on commit-tip
		{Authority: AuthoritySignedPromotionTag, Signatures: SignaturesOff},                                                     // no prefix
		{Authority: AuthoritySignedPromotionTag, Signatures: SignaturesOff, PromotionTagPrefix: "refs/tags/promote/prd/"},       // wrong root
		{Authority: AuthoritySignedPromotionTag, Signatures: SignaturesOff, PromotionTagPrefix: "refs/tags/fxvcs/promote/prd"},  // no trailing slash
		{Authority: AuthoritySignedPromotionTag, Signatures: SignaturesOff, PromotionTagPrefix: "refs/tags/fxvcs/promote/PRD/"}, // bad target
		{Authority: AuthoritySignedCommitTip, Signatures: "maybe"},
	}
	for _, p := range bad {
		if err := ValidatePolicy(p); !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("%+v accepted: %v", p, err)
		}
	}
}

func TestFloorCannotBeRelaxed(t *testing.T) {
	local := domain.CoordinationTrust{Authority: AuthoritySignedCommitTip, Signatures: SignaturesRequired, AllowedSignersFile: "/etc/fxvcs/allowed_signers"}
	if _, err := Floor(local, domain.CoordinationTrust{Signatures: SignaturesOff}); !errors.Is(err, ErrDesiredStateRelaxes) {
		t.Fatalf("relaxation accepted: %v", err)
	}
	if _, err := Floor(local, domain.CoordinationTrust{Authority: AuthoritySignedPromotionTag}); !errors.Is(err, ErrDesiredStateRelaxes) {
		t.Fatalf("authority switch accepted: %v", err)
	}
	if _, err := Floor(local, domain.CoordinationTrust{AllowedSignersFile: "/tmp/evil"}); !errors.Is(err, ErrDesiredStateRelaxes) {
		t.Fatalf("signers override accepted: %v", err)
	}
	loose := domain.CoordinationTrust{Authority: AuthoritySignedCommitTip, Signatures: SignaturesOptional, AllowedSignersFile: "/etc/fxvcs/allowed_signers"}
	eff, err := Floor(loose, domain.CoordinationTrust{Signatures: SignaturesRequired})
	if err != nil || eff.Signatures != SignaturesRequired {
		t.Fatalf("tightening rejected: %v %+v", err, eff)
	}
}

func TestPromotionTagParsing(t *testing.T) {
	for _, ref := range []string{"refs/tags/fxvcs/promote/prd/0", "refs/tags/fxvcs/promote/prd/1", "refs/tags/fxvcs/promote/prd/123456789"} {
		if _, err := ParsePromotionTag("prd", ref); err != nil {
			t.Errorf("%s: %v", ref, err)
		}
	}
	cases := map[string]error{
		"refs/tags/fxvcs/promote/prd/01":                    ErrNonCanonicalSeq,
		"refs/tags/fxvcs/promote/prd/+1":                    ErrNonCanonicalSeq,
		"refs/tags/fxvcs/promote/prd/1.0":                   ErrNonCanonicalSeq,
		"refs/tags/fxvcs/promote/prd/":                      ErrNonCanonicalSeq,
		"refs/tags/fxvcs/promote/prd/1/2":                   ErrNonCanonicalSeq,
		"refs/tags/fxvcs/promote/prd/999999999999999999999": ErrNonCanonicalSeq,
		"refs/tags/fxvcs/promote/dev/1":                     ErrNotPromotionTag,
		"refs/tags/fxvcs/promote/prd-old/1":                 ErrNotPromotionTag,
		"refs/heads/main":                                   ErrNotPromotionTag,
	}
	for ref, want := range cases {
		if _, err := ParsePromotionTag("prd", ref); !errors.Is(err, want) {
			t.Errorf("%s: got %v want %v", ref, err, want)
		}
	}
	if _, err := ParsePromotionTag("Bad Target", "x"); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatal("bad target accepted")
	}
}

func TestSelectDesired(t *testing.T) {
	mk := func(seq uint64, valid bool, commit string) Candidate {
		return Candidate{Tag: PromotionTag{Target: "prd", Seq: seq}, Valid: valid, Commit: commit}
	}
	// Highest VALID wins even if a newer invalid tag exists (an unauthorized tag never moves state).
	got, err := SelectDesired([]Candidate{mk(3, true, "c3"), mk(7, false, "c7"), mk(5, true, "c5")})
	if err != nil || got.Commit != "c5" {
		t.Fatalf("got %+v %v", got, err)
	}
	// Rollback by newer tag selecting older commit is just "highest valid".
	got, _ = SelectDesired([]Candidate{mk(8, true, "c3"), mk(5, true, "c5")})
	if got.Commit != "c3" {
		t.Fatal("rollback promotion not selected")
	}
	if _, err := SelectDesired([]Candidate{mk(1, false, "x")}); !errors.Is(err, ErrNoValidPromotion) {
		t.Fatal("expected no valid promotion")
	}
	if _, err := SelectDesired([]Candidate{mk(1, true, "a"), mk(1, true, "b")}); !errors.Is(err, ErrDuplicateSeq) {
		t.Fatal("duplicate seq accepted")
	}
	if _, err := SelectDesired(nil); !errors.Is(err, ErrNoValidPromotion) {
		t.Fatal("empty accepted")
	}
}

func TestThresholdPolicy(t *testing.T) {
	p := ThresholdPolicy{RequiredRoles: []string{"creator", "cloud"}, Threshold: 2, RevokedKeyIDs: []string{"SHA256:revoked"}}
	ok, _ := p.Satisfied([]Signer{{Role: "creator", KeyID: "SHA256:a"}, {Role: "cloud", KeyID: "SHA256:b"}})
	if !ok {
		t.Fatal("expected satisfied")
	}
	if ok, why := p.Satisfied([]Signer{{Role: "creator", KeyID: "SHA256:a"}}); ok || why == "" {
		t.Fatal("missing role accepted")
	}
	if ok, _ := p.Satisfied([]Signer{{Role: "creator", KeyID: "SHA256:a"}, {Role: "cloud", KeyID: "SHA256:revoked"}}); ok {
		t.Fatal("revoked key counted")
	}
	// Same key signing twice under two roles does not meet a threshold of 2 distinct signers.
	if ok, _ := p.Satisfied([]Signer{{Role: "creator", KeyID: "SHA256:a"}, {Role: "cloud", KeyID: "SHA256:a"}}); ok {
		t.Fatal("single key counted twice")
	}
	if string(SignedPayload("sha256:ab")) != "fxvcs-release-v1\nsha256:ab\n" {
		t.Fatal("payload changed")
	}
}
