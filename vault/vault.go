// Package vault is Fliable's built-in secrets store: named string secrets
// encrypted at rest with AES-256-GCM and persisted through the engine's
// store (memory, journal or SQL). Connector credentials, API tokens and
// OAuth client secrets live here instead of inside process models.
//
// The master key comes from the embedding application (typically an
// environment variable or a mounted file) and never touches the store;
// each value is sealed with a fresh random nonce. Zero external
// dependencies — crypto/aes + crypto/cipher from the standard library.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/olbboy/fliable/store"
)

// BlobKind is the store blob namespace used for sealed secrets.
const BlobKind = "secret"

// ErrNotFound is returned when a secret does not exist.
var ErrNotFound = errors.New("vault: secret not found")

// Vault seals and unseals named secrets against a Store.
type Vault struct {
	st   store.Store
	aead cipher.AEAD
}

// New creates a vault over the store. masterKey may be any length; it is
// stretched to a 256-bit AES key with SHA-256. An empty key is rejected.
func New(st store.Store, masterKey []byte) (*Vault, error) {
	if len(masterKey) == 0 {
		return nil, errors.New("vault: empty master key")
	}
	sum := sha256.Sum256(masterKey)
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{st: st, aead: aead}, nil
}

// Set seals and stores a secret value under name.
func (v *Vault) Set(name, value string) error {
	if name == "" {
		return errors.New("vault: empty secret name")
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	// The name is bound as additional data, so a ciphertext copied onto
	// another key fails to open.
	sealed := v.aead.Seal(nonce, nonce, []byte(value), []byte(name))
	return v.st.PutBlob(&store.Blob{
		Kind: BlobKind, Key: name, Data: sealed, UpdatedAt: time.Now().UTC(),
	})
}

// Get unseals the named secret.
func (v *Vault) Get(name string) (string, error) {
	b, err := v.st.GetBlob(BlobKind, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	ns := v.aead.NonceSize()
	if len(b.Data) < ns {
		return "", fmt.Errorf("vault: secret %q is corrupt", name)
	}
	plain, err := v.aead.Open(nil, b.Data[:ns], b.Data[ns:], []byte(name))
	if err != nil {
		return "", fmt.Errorf("vault: cannot unseal %q (wrong master key?)", name)
	}
	return string(plain), nil
}

// List returns the names of all stored secrets (never the values).
func (v *Vault) List() ([]string, error) {
	blobs, err := v.st.ListBlobs(BlobKind)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(blobs))
	for i, b := range blobs {
		names[i] = b.Key
	}
	return names, nil
}

// Delete removes a secret.
func (v *Vault) Delete(name string) error { return v.st.DeleteBlob(BlobKind, name) }

// Secret implements engine.SecretSource, so a Vault plugs straight into
// engine.WithSecrets.
func (v *Vault) Secret(name string) (string, error) { return v.Get(name) }
