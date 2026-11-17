// Package msgcrypto provides the small, reusable encryption primitive used by
// h8s services. It intentionally knows nothing about NATS subjects, HTTP
// headers, or proxy policy.
//
// The wire format is NATS nkeys' curve format: "xkv1" || 24-byte random nonce
// || NaCl box ciphertext. nkeys.Seal generates a fresh nonce for every call
// and nkeys.Open validates the version, nonce, sender public key, and
// Poly1305 tag.
package msgcrypto

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nats-io/nkeys"
)

var (
	// ErrInvalidRecipient is returned when a public key is missing or not a
	// valid nkeys curve public key.
	ErrInvalidRecipient = errors.New("msgcrypto: invalid recipient curve public key")
	// ErrInvalidSender is returned when a sender public key is missing or not
	// a valid nkeys curve public key.
	ErrInvalidSender = errors.New("msgcrypto: invalid sender curve public key")
	// ErrDecrypt is returned when a sealed message cannot be opened.
	ErrDecrypt = errors.New("msgcrypto: unable to decrypt message")
)

// Key is a curve keypair. It is safe for concurrent Seal and Open calls; Wipe
// must only be called after all users have stopped.
type Key struct {
	kp        nkeys.KeyPair
	publicKey string
}

// Generate creates a new ephemeral curve keypair.
func Generate() (*Key, error) {
	kp, err := nkeys.CreateCurveKeys()
	if err != nil {
		return nil, fmt.Errorf("msgcrypto: generate curve key: %w", err)
	}
	return newKey(kp)
}

// FromSeed creates a keypair from an encoded curve seed (the "SX..." form
// returned by nkeys.KeyPair.Seed).
func FromSeed(seed []byte) (*Key, error) {
	kp, err := nkeys.FromCurveSeed(seed)
	if err != nil {
		return nil, fmt.Errorf("msgcrypto: invalid curve seed: %w", err)
	}
	return newKey(kp)
}

// FromSeedFile loads an encoded curve seed from path. The file must not be
// group- or world-readable.
func FromSeedFile(path string) (*Key, error) {
	seed, err := ReadSeedFile(path)
	if err != nil {
		return nil, err
	}
	defer zero(seed)
	return FromSeed(seed)
}

// ReadSeedFile reads and trims an encoded curve seed. It rejects empty files
// and files with group/world permission bits.
func ReadSeedFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("msgcrypto: stat seed file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("msgcrypto: seed file %s must not be group/world readable (mode %04o)", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("msgcrypto: read seed file: %w", err)
	}
	seed := []byte(strings.TrimSpace(string(data)))
	if len(seed) == 0 {
		return nil, errors.New("msgcrypto: seed file is empty")
	}
	return seed, nil
}

func newKey(kp nkeys.KeyPair) (*Key, error) {
	publicKey, err := kp.PublicKey()
	if err != nil {
		kp.Wipe()
		return nil, fmt.Errorf("msgcrypto: derive public key: %w", err)
	}
	if !nkeys.IsValidPublicCurveKey(publicKey) {
		kp.Wipe()
		return nil, ErrInvalidRecipient
	}
	return &Key{kp: kp, publicKey: publicKey}, nil
}

// PublicKey returns the encoded curve public key (the "X..." form).
func (k *Key) PublicKey() string {
	if k == nil {
		return ""
	}
	return k.publicKey
}

// Seal encrypts data to recipient. The recipient must be a valid "X..." curve
// public key.
func (k *Key) Seal(data []byte, recipient string) ([]byte, error) {
	if k == nil || k.kp == nil {
		return nil, errors.New("msgcrypto: key is not initialized")
	}
	if recipient == "" || !nkeys.IsValidPublicCurveKey(recipient) {
		return nil, ErrInvalidRecipient
	}
	sealed, err := k.kp.Seal(data, recipient)
	if err != nil {
		return nil, fmt.Errorf("msgcrypto: seal: %w", err)
	}
	return sealed, nil
}

// Open decrypts data from sender. The sender must be a valid "X..." curve
// public key. A truncated, tampered, or wrong-sender message returns
// ErrDecrypt.
func (k *Key) Open(data []byte, sender string) ([]byte, error) {
	if k == nil || k.kp == nil {
		return nil, errors.New("msgcrypto: key is not initialized")
	}
	if sender == "" || !nkeys.IsValidPublicCurveKey(sender) {
		return nil, ErrInvalidSender
	}
	plain, err := k.kp.Open(data, sender)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	return plain, nil
}

// Wipe overwrites the private key material on a best-effort basis.
func (k *Key) Wipe() {
	if k == nil {
		return
	}
	if k.kp != nil {
		k.kp.Wipe()
	}
	k.publicKey = ""
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
