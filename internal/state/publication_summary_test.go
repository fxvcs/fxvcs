package state

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPublicationSummaryUsesRequiredRemoteCoverage(t *testing.T) {
	ctx := context.Background()
	l, err := OpenLedger(ctx, filepath.Join(t.TempDir(), "publication.db"), testOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	recs := []ObjectRecord{
		{Digest: oidA, StorageDomainID: "domain_fixture", Size: 10, Encoding: "whole/1"},
		{Digest: oidB, StorageDomainID: "domain_fixture", Size: 20, Encoding: "whole/1"},
		{Digest: oidC, StorageDomainID: "domain_fixture", Size: 30, Encoding: "whole/1"},
	}
	for _, rec := range recs {
		if err := l.RecordIngest(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.MarkPublishedMany(ctx, map[string][]string{
		oidA: {"primary"},
		oidB: {"primary", "backup"},
	}); err != nil {
		t.Fatal(err)
	}
	// Settle the original primary obligation. A summary for a newly required
	// backup must still report oidA even though its pin bit is now clear.
	if _, err := l.Settle(ctx, []string{"primary"}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		remotes  []string
		pending  int
		pendByte int64
	}{
		{name: "no required destination", remotes: nil, pending: 0, pendByte: 0},
		{name: "primary", remotes: []string{"primary"}, pending: 1, pendByte: 30},
		{name: "primary and backup", remotes: []string{"primary", "backup"}, pending: 2, pendByte: 40},
		{name: "duplicate names do not inflate coverage", remotes: []string{"primary", "primary"}, pending: 1, pendByte: 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := l.Summary(ctx, tc.remotes)
			if err != nil {
				t.Fatal(err)
			}
			if got.Objects != 3 || got.Bytes != 60 || got.PendingObjects != tc.pending || got.PendingBytes != tc.pendByte {
				t.Fatalf("Summary(%v) = %+v", tc.remotes, got)
			}
			usage := map[string]PublicationRemoteUsage{}
			for _, remote := range got.Remotes {
				usage[remote.Name] = remote
			}
			if usage["primary"].Objects != 2 || usage["primary"].Bytes != 30 ||
				usage["backup"].Objects != 1 || usage["backup"].Bytes != 20 {
				t.Fatalf("remote usage = %+v", got.Remotes)
			}
		})
	}
}

func TestPublicationSummaryAndReferrersManyAreEmptyWithoutRows(t *testing.T) {
	ctx := context.Background()
	l, err := OpenLedger(ctx, filepath.Join(t.TempDir(), "publication.db"), testOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	got, err := l.Summary(ctx, []string{"primary"})
	if err != nil || got.Objects != 0 || got.Bytes != 0 || got.PendingObjects != 0 || got.PendingBytes != 0 || len(got.Remotes) != 0 {
		t.Fatalf("empty summary = %+v, %v", got, err)
	}
	refs, err := l.ReferrersMany(ctx, nil)
	if err != nil || len(refs) != 0 {
		t.Fatalf("empty referrers = %+v, %v", refs, err)
	}
}

func TestReferrersManyMatchesSingleObjectOrdering(t *testing.T) {
	ctx := context.Background()
	l, err := OpenLedger(ctx, filepath.Join(t.TempDir(), "publication.db"), testOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, rec := range []ObjectRecord{
		{Digest: oidA, StorageDomainID: "domain_fixture", Size: 10, Encoding: "whole/1"},
		{Digest: oidB, StorageDomainID: "domain_fixture", Size: 20, Encoding: "whole/1"},
	} {
		if err := l.RecordIngest(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range [][3]string{
		{oidA, "z.bin", "index"},
		{oidA, "a.bin", "commit-b"},
		{oidA, "a.bin", "commit-a"},
		{oidB, "b.bin", "index"},
	} {
		if err := l.AddReferrer(ctx, ref[0], ref[1], ref[2]); err != nil {
			t.Fatal(err)
		}
	}
	many, err := l.ReferrersMany(ctx, []string{oidA, oidB})
	if err != nil {
		t.Fatal(err)
	}
	for _, digest := range []string{oidA, oidB} {
		one, err := l.Referrers(ctx, digest)
		if err != nil {
			t.Fatal(err)
		}
		if len(many[digest]) != len(one) {
			t.Fatalf("%s: many=%v one=%v", digest, many[digest], one)
		}
		for i := range one {
			if many[digest][i] != one[i] {
				t.Fatalf("%s[%d]: many=%v one=%v", digest, i, many[digest][i], one[i])
			}
		}
	}
}
