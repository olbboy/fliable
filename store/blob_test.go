package store

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func testBlobCRUD(t *testing.T, st Store) {
	t.Helper()
	b := &Blob{Kind: "form", Key: "approve", Data: []byte(`{"fields":[]}`), UpdatedAt: time.Now().UTC()}
	if err := st.PutBlob(b); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.PutBlob(&Blob{Kind: "secret", Key: "apiToken", Data: []byte("ciphertext")}); err != nil {
		t.Fatalf("put 2: %v", err)
	}

	got, err := st.GetBlob("form", "approve")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got.Data, b.Data) {
		t.Fatalf("data mismatch: %q", got.Data)
	}
	// Mutating the returned copy must not touch stored state.
	got.Data[0] = 'X'
	again, _ := st.GetBlob("form", "approve")
	if !bytes.Equal(again.Data, b.Data) {
		t.Fatal("returned blob aliases stored state")
	}

	forms, err := st.ListBlobs("form")
	if err != nil || len(forms) != 1 {
		t.Fatalf("list form: %v %d", err, len(forms))
	}
	all, _ := st.ListBlobs("")
	if len(all) != 2 {
		t.Fatalf("list all: %d", len(all))
	}

	if err := st.DeleteBlob("form", "approve"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetBlob("form", "approve"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMemoryBlobs(t *testing.T) { testBlobCRUD(t, NewMemory()) }

func TestJournalBlobsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	testBlobCRUD(t, j)
	if err := j.PutBlob(&Blob{Kind: "webhook", Key: "orders", Data: []byte(`{"message":"orderReceived"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: journal replay must restore the surviving blobs.
	j2, err := OpenJournal(dir, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if _, err := j2.GetBlob("form", "approve"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted blob resurrected: %v", err)
	}
	for _, want := range []struct{ kind, key string }{{"secret", "apiToken"}, {"webhook", "orders"}} {
		if _, err := j2.GetBlob(want.kind, want.key); err != nil {
			t.Fatalf("blob %s/%s lost after restart: %v", want.kind, want.key, err)
		}
	}

	// And through snapshot compaction too.
	if err := j2.Compact(); err != nil {
		t.Fatal(err)
	}
	if err := j2.Close(); err != nil {
		t.Fatal(err)
	}
	j3, err := OpenJournal(dir, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer j3.Close()
	if _, err := j3.GetBlob("secret", "apiToken"); err != nil {
		t.Fatalf("blob lost after compaction: %v", err)
	}
}
