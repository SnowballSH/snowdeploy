package journal

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Journal {
	t.Helper()
	j, err := Open(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func sample() Entry {
	return Entry{
		Service:   "web",
		Action:    ActionDeploy,
		Actor:     "admin",
		OldDigest: "sha256:aaa",
		NewDigest: "sha256:bbb",
		PRNumber:  42,
		MergeSHA:  "deadbeef",
		State:     "pr-open",
		Detail:    "opened",
		StartedAt: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
	}
}

func TestBeginFinishRoundTrip(t *testing.T) {
	j := open(t)
	id, err := j.Begin(sample())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if id == 0 {
		t.Fatal("Begin returned id 0")
	}
	if err := j.Finish(id, StateHealthy, "probe green"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	got, err := j.Recent("web", 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Recent returned %d entries, want 1", len(got))
	}
	e := got[0]
	want := sample()
	if e.ID != id || e.Service != want.Service || e.Action != want.Action ||
		e.Actor != want.Actor || e.OldDigest != want.OldDigest ||
		e.NewDigest != want.NewDigest || e.PRNumber != want.PRNumber ||
		e.MergeSHA != want.MergeSHA {
		t.Errorf("round trip lost a field: %+v", e)
	}
	if e.State != StateHealthy {
		t.Errorf("State = %q, want %q", e.State, StateHealthy)
	}
	if e.Detail != "probe green" {
		t.Errorf("Detail = %q", e.Detail)
	}
	if !e.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", e.StartedAt, want.StartedAt)
	}
	if e.FinishedAt.IsZero() {
		t.Error("FinishedAt is zero after Finish")
	}
}

func TestUnfinishedEntryHasZeroFinishedAt(t *testing.T) {
	j := open(t)
	if _, err := j.Begin(sample()); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	got, err := j.Recent("web", 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if !got[0].FinishedAt.IsZero() {
		t.Errorf("FinishedAt = %v on an unfinished entry, want zero", got[0].FinishedAt)
	}
}

func TestRecentIsNewestFirstAndBounded(t *testing.T) {
	j := open(t)
	for i := range 5 {
		e := sample()
		e.NewDigest = "sha256:" + string(rune('a'+i))
		e.StartedAt = e.StartedAt.Add(time.Duration(i) * time.Minute)
		if _, err := j.Begin(e); err != nil {
			t.Fatalf("Begin %d: %v", i, err)
		}
	}
	got, err := j.Recent("web", 3)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Recent(3) returned %d", len(got))
	}
	if got[0].NewDigest != "sha256:e" || got[2].NewDigest != "sha256:c" {
		t.Errorf("not newest-first: %q .. %q", got[0].NewDigest, got[2].NewDigest)
	}
}

func TestRecentFiltersByService(t *testing.T) {
	j := open(t)
	if _, err := j.Begin(sample()); err != nil {
		t.Fatal(err)
	}
	other := sample()
	other.Service = "api"
	if _, err := j.Begin(other); err != nil {
		t.Fatal(err)
	}
	got, err := j.Recent("api", 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 || got[0].Service != "api" {
		t.Errorf("service filter leaked: %+v", got)
	}
}

func TestLastHealthyDigest(t *testing.T) {
	j := open(t)

	if _, err := j.LastHealthyDigest("web"); !errors.Is(err, ErrNoHealthyDeploy) {
		t.Fatalf("empty journal: err = %v, want ErrNoHealthyDeploy", err)
	}

	first := sample()
	first.NewDigest = "sha256:one"
	id1, _ := j.Begin(first)
	if err := j.Finish(id1, StateHealthy, ""); err != nil {
		t.Fatal(err)
	}

	second := sample()
	second.NewDigest = "sha256:two"
	second.StartedAt = second.StartedAt.Add(time.Minute)
	id2, _ := j.Begin(second)
	if err := j.Finish(id2, StateFailed, "probe red"); err != nil {
		t.Fatal(err)
	}

	got, err := j.LastHealthyDigest("web")
	if err != nil {
		t.Fatalf("LastHealthyDigest: %v", err)
	}
	if got != "sha256:one" {
		t.Errorf("LastHealthyDigest = %q, want sha256:one (the failed one must not win)", got)
	}
}

func TestPreviousHealthyDigestSkipsTheCurrentOne(t *testing.T) {
	j := open(t)

	for i, d := range []string{"sha256:one", "sha256:two", "sha256:three"} {
		e := sample()
		e.NewDigest = d
		e.StartedAt = e.StartedAt.Add(time.Duration(i) * time.Minute)
		id, _ := j.Begin(e)
		if err := j.Finish(id, StateHealthy, ""); err != nil {
			t.Fatal(err)
		}
	}

	got, err := j.PreviousHealthyDigest("web", "sha256:three")
	if err != nil {
		t.Fatalf("PreviousHealthyDigest: %v", err)
	}
	if got != "sha256:two" {
		t.Errorf("PreviousHealthyDigest = %q, want sha256:two", got)
	}

	if _, err := j.PreviousHealthyDigest("web", "sha256:nothing-matches-here"); err != nil {
		t.Errorf("excluding an unrelated digest should still find one: %v", err)
	}
}

func TestPreviousHealthyDigestWithOnlyTheCurrentOne(t *testing.T) {
	j := open(t)
	e := sample()
	e.NewDigest = "sha256:only"
	id, _ := j.Begin(e)
	if err := j.Finish(id, StateHealthy, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := j.PreviousHealthyDigest("web", "sha256:only"); !errors.Is(err, ErrNoHealthyDeploy) {
		t.Fatalf("err = %v, want ErrNoHealthyDeploy", err)
	}
}

func TestUnfinishedReturnsOnlyOpenRowsOldestFirst(t *testing.T) {
	j := open(t)

	finished := sample()
	idDone, _ := j.Begin(finished)
	if err := j.Finish(idDone, StateHealthy, ""); err != nil {
		t.Fatal(err)
	}

	older := sample()
	older.Service = "api"
	idOlder, _ := j.Begin(older)

	newer := sample()
	newer.StartedAt = newer.StartedAt.Add(time.Hour)
	idNewer, _ := j.Begin(newer)

	got, err := j.Unfinished()
	if err != nil {
		t.Fatalf("Unfinished: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Unfinished returned %d entries, want the 2 open ones: %+v", len(got), got)
	}
	if got[0].ID != idOlder || got[1].ID != idNewer {
		t.Errorf("not oldest-first: %d, %d (want %d, %d)", got[0].ID, got[1].ID, idOlder, idNewer)
	}
	for _, e := range got {
		if e.ID == idDone {
			t.Errorf("a finished entry was reported as unfinished: %+v", e)
		}
		if !e.FinishedAt.IsZero() {
			t.Errorf("an unfinished entry carries a finish time: %+v", e)
		}
	}
}

func TestUnfinishedOnAnEmptyJournal(t *testing.T) {
	j := open(t)
	got, err := j.Unfinished()
	if err != nil {
		t.Fatalf("Unfinished: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty journal reported unfinished entries: %+v", got)
	}
}

func TestFinishedRowIsImmutable(t *testing.T) {
	j := open(t)
	id, _ := j.Begin(sample())
	if err := j.Finish(id, StateHealthy, "first"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := j.Finish(id, StateFailed, "rewrite"); !errors.Is(err, ErrAlreadyFinished) {
		t.Fatalf("second Finish: err = %v, want ErrAlreadyFinished", err)
	}
	if err := j.Progress(id, "reconciling", "rewrite"); !errors.Is(err, ErrAlreadyFinished) {
		t.Fatalf("Progress after Finish: err = %v, want ErrAlreadyFinished", err)
	}

	got, _ := j.Recent("web", 1)
	if got[0].State != StateHealthy || got[0].Detail != "first" {
		t.Errorf("receipt was rewritten: %+v", got[0])
	}
}

func TestProgressUpdatesUnfinishedRow(t *testing.T) {
	j := open(t)
	id, _ := j.Begin(sample())
	if err := j.Progress(id, "reconciling", "rendering unit"); err != nil {
		t.Fatalf("Progress: %v", err)
	}
	got, _ := j.Recent("web", 1)
	if got[0].State != "reconciling" || got[0].Detail != "rendering unit" {
		t.Errorf("Progress did not land: %+v", got[0])
	}
	if !got[0].FinishedAt.IsZero() {
		t.Error("Progress set FinishedAt")
	}
}

func TestFinishUnknownID(t *testing.T) {
	j := open(t)
	if err := j.Finish(9999, StateHealthy, ""); err == nil {
		t.Fatal("Finish on an unknown id succeeded")
	}
}

func TestOpenIsIdempotentAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.db")

	j1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id, _ := j1.Begin(sample())
	if err := j1.Finish(id, StateHealthy, "kept"); err != nil {
		t.Fatal(err)
	}
	if err := j1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	j2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = j2.Close() }()

	got, err := j2.Recent("web", 10)
	if err != nil {
		t.Fatalf("Recent after reopen: %v", err)
	}
	if len(got) != 1 || got[0].Detail != "kept" {
		t.Errorf("journal did not survive restart: %+v", got)
	}
}

func TestSetDigestsRewritesAnUnfinishedEntry(t *testing.T) {
	j := open(t)
	id, err := j.Begin(sample())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := j.SetDigests(id, "sha256:ccc", "sha256:ccc"); err != nil {
		t.Fatalf("SetDigests: %v", err)
	}
	got, err := j.Recent("web", 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if got[0].OldDigest != "sha256:ccc" || got[0].NewDigest != "sha256:ccc" {
		t.Errorf("digests = %s -> %s, want the rewritten pin twice",
			got[0].OldDigest, got[0].NewDigest)
	}
}

func TestSetDigestsRefusesAFinishedReceipt(t *testing.T) {
	j := open(t)
	id, err := j.Begin(sample())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := j.Finish(id, StateHealthy, "done"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := j.SetDigests(id, "sha256:ccc", "sha256:ccc"); !errors.Is(err, ErrAlreadyFinished) {
		t.Fatalf("SetDigests on a receipt = %v, want ErrAlreadyFinished", err)
	}
}
