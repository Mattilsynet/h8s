package msgcrypto

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRoundTrip(t *testing.T) {
	sender, err := Generate()
	require.NoError(t, err)
	recipient, err := Generate()
	require.NoError(t, err)
	defer sender.Wipe()
	defer recipient.Wipe()

	plaintext := []byte("hello h8s")
	sealed, err := sender.Seal(plaintext, recipient.PublicKey())
	require.NoError(t, err)
	require.NotEqual(t, plaintext, sealed)

	opened, err := recipient.Open(sealed, sender.PublicKey())
	require.NoError(t, err)
	require.Equal(t, plaintext, opened)
}

func TestEmptyPlaintextRoundTrip(t *testing.T) {
	sender, err := Generate()
	require.NoError(t, err)
	recipient, err := Generate()
	require.NoError(t, err)
	defer sender.Wipe()
	defer recipient.Wipe()

	sealed, err := sender.Seal(nil, recipient.PublicKey())
	require.NoError(t, err)
	require.NotEmpty(t, sealed, "sealed empty messages still carry version, nonce and tag")

	opened, err := recipient.Open(sealed, sender.PublicKey())
	require.NoError(t, err)
	require.Empty(t, opened)
}

func TestWrongKeyFails(t *testing.T) {
	sender, err := Generate()
	require.NoError(t, err)
	recipient, err := Generate()
	require.NoError(t, err)
	wrongRecipient, err := Generate()
	require.NoError(t, err)
	defer sender.Wipe()
	defer recipient.Wipe()
	defer wrongRecipient.Wipe()

	sealed, err := sender.Seal([]byte("secret"), recipient.PublicKey())
	require.NoError(t, err)

	_, err = wrongRecipient.Open(sealed, sender.PublicKey())
	require.ErrorIs(t, err, ErrDecrypt)
}

func TestTamperedCiphertextFails(t *testing.T) {
	sender, err := Generate()
	require.NoError(t, err)
	recipient, err := Generate()
	require.NoError(t, err)
	defer sender.Wipe()
	defer recipient.Wipe()

	sealed, err := sender.Seal([]byte("secret"), recipient.PublicKey())
	require.NoError(t, err)
	sealed[len(sealed)-1] ^= 0xff

	_, err = recipient.Open(sealed, sender.PublicKey())
	require.ErrorIs(t, err, ErrDecrypt)
}

func TestTruncatedCiphertextFails(t *testing.T) {
	sender, err := Generate()
	require.NoError(t, err)
	recipient, err := Generate()
	require.NoError(t, err)
	defer sender.Wipe()
	defer recipient.Wipe()

	sealed, err := sender.Seal([]byte("secret"), recipient.PublicKey())
	require.NoError(t, err)

	_, err = recipient.Open(sealed[:10], sender.PublicKey())
	require.Error(t, err)
}

func TestInvalidKeys(t *testing.T) {
	key, err := Generate()
	require.NoError(t, err)
	defer key.Wipe()

	_, err = key.Seal([]byte("x"), "not-a-key")
	require.ErrorIs(t, err, ErrInvalidRecipient)

	_, err = key.Open([]byte("x"), "not-a-key")
	require.ErrorIs(t, err, ErrInvalidSender)
}

func TestSeedRoundTrip(t *testing.T) {
	key, err := Generate()
	require.NoError(t, err)
	defer key.Wipe()

	seed, err := key.kp.Seed()
	require.NoError(t, err)
	defer zero(seed)

	fromSeed, err := FromSeed(seed)
	require.NoError(t, err)
	defer fromSeed.Wipe()
	require.Equal(t, key.PublicKey(), fromSeed.PublicKey())
}

func TestFromSeedFileRejectsLoosePermissions(t *testing.T) {
	key, err := Generate()
	require.NoError(t, err)
	defer key.Wipe()
	seed, err := key.kp.Seed()
	require.NoError(t, err)
	defer zero(seed)

	path := filepath.Join(t.TempDir(), "seed")
	require.NoError(t, os.WriteFile(path, seed, 0o644))

	_, err = FromSeedFile(path)
	require.Error(t, err)

	require.NoError(t, os.Chmod(path, 0o600))
	loaded, err := FromSeedFile(path)
	require.NoError(t, err)
	defer loaded.Wipe()
	require.Equal(t, key.PublicKey(), loaded.PublicKey())
}

func TestNilKey(t *testing.T) {
	var key *Key
	_, err := key.Seal([]byte("x"), "X")
	require.Equal(t, errors.New("msgcrypto: key is not initialized").Error(), err.Error())
	require.Empty(t, key.PublicKey())
	key.Wipe()
}
