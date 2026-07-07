package vault

import (
	"errors"
	"strings"
	"testing"

	"github.com/olbboy/fliable/store"
)

func TestVaultRoundTrip(t *testing.T) {
	st := store.NewMemory()
	v, err := New(st, []byte("master-key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("slackToken", "xoxb-secret"); err != nil {
		t.Fatal(err)
	}
	got, err := v.Get("slackToken")
	if err != nil || got != "xoxb-secret" {
		t.Fatalf("get: %q %v", got, err)
	}

	// The stored blob must not contain the plaintext.
	b, err := st.GetBlob(BlobKind, "slackToken")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b.Data), "xoxb-secret") {
		t.Fatal("secret stored in plaintext")
	}

	names, _ := v.List()
	if len(names) != 1 || names[0] != "slackToken" {
		t.Fatalf("list: %v", names)
	}

	if err := v.Delete("slackToken"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Get("slackToken"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestVaultWrongKeyAndTamper(t *testing.T) {
	st := store.NewMemory()
	v1, _ := New(st, []byte("key-one"))
	if err := v1.Set("db", "postgres://user:pw@host"); err != nil {
		t.Fatal(err)
	}

	// Wrong master key cannot unseal.
	v2, _ := New(st, []byte("key-two"))
	if _, err := v2.Get("db"); err == nil {
		t.Fatal("wrong key unsealed the secret")
	}

	// A ciphertext moved to a different name fails (name is bound as AAD).
	b, _ := st.GetBlob(BlobKind, "db")
	b.Key = "other"
	_ = st.PutBlob(b)
	if _, err := v1.Get("other"); err == nil {
		t.Fatal("renamed ciphertext unsealed")
	}

	// Same-name unseal still works.
	if got, err := v1.Get("db"); err != nil || got != "postgres://user:pw@host" {
		t.Fatalf("original secret broken: %q %v", got, err)
	}
}

func TestVaultSurvivesJournalRestart(t *testing.T) {
	dir := t.TempDir()
	j, err := store.OpenJournal(dir, store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := New(j, []byte("k"))
	if err := v.Set("api", "value-1"); err != nil {
		t.Fatal(err)
	}
	j.Close()

	j2, err := store.OpenJournal(dir, store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	v2, _ := New(j2, []byte("k"))
	if got, err := v2.Get("api"); err != nil || got != "value-1" {
		t.Fatalf("after restart: %q %v", got, err)
	}
}
