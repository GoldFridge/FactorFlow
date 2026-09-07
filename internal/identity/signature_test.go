package identity_test

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/sha3"

	"github.com/GoldFridge/factorflow/internal/identity"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

// wallet is a throwaway key pair standing in for a browser wallet. The tests sign with it
// exactly as personal_sign does, so what is verified here is the real scheme rather than a
// re-implementation agreeing with itself.
type wallet struct {
	key     *secp256k1.PrivateKey
	address string
}

func newWallet(t *testing.T) *wallet {
	t.Helper()

	key, err := secp256k1.GeneratePrivateKey()
	require.NoError(t, err)

	uncompressed := key.PubKey().SerializeUncompressed()
	digest := keccak(uncompressed[1:])
	return &wallet{key: key, address: "0x" + hex.EncodeToString(digest[12:])}
}

// sign produces the 65-byte r||s||v signature a wallet returns from personal_sign.
func (w *wallet) sign(t *testing.T, message string) string {
	t.Helper()

	prefixed := "\x19Ethereum Signed Message:\n" + itoa(len(message)) + message
	compact := ecdsa.SignCompact(w.key, keccak([]byte(prefixed)), false)

	// SignCompact puts the recovery byte first; Ethereum puts it last.
	signature := make([]byte, 65)
	copy(signature, compact[1:])
	signature[64] = compact[0] - 27 + 27
	return "0x" + hex.EncodeToString(signature)
}

func keccak(data []byte) []byte {
	hash := sha3.NewLegacyKeccak256()
	hash.Write(data)
	return hash.Sum(nil)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestRecoverAddress(t *testing.T) {
	t.Parallel()

	w := newWallet(t)
	message := "FactorFlow wants you to sign in with your wallet.\n\nNonce: abc123"

	recovered, err := identity.RecoverAddress(message, w.sign(t, message))
	require.NoError(t, err)
	assert.Equal(t, strings.ToLower(w.address), strings.ToLower(recovered))
}

func TestVerifySignature(t *testing.T) {
	t.Parallel()

	w := newWallet(t)
	message := "sign in please"
	signature := w.sign(t, message)

	require.NoError(t, identity.VerifySignature(w.address, message, signature))
	require.NoError(t, identity.VerifySignature(strings.ToUpper(w.address), message, signature),
		"an address is compared case-insensitively")
}

// TestSignatureForAnotherMessageIsRefused is the whole point of the nonce: a signature is
// evidence about one message, not a password.
func TestSignatureForAnotherMessageIsRefused(t *testing.T) {
	t.Parallel()

	w := newWallet(t)
	signature := w.sign(t, "sign in to FactorFlow, nonce abc")

	err := identity.VerifySignature(w.address, "sign in to FactorFlow, nonce xyz", signature)
	require.ErrorIs(t, err, apperr.ErrValidation)
}

// TestAnotherWalletsSignatureIsRefused is the check that stops one wallet claiming another's
// identity by presenting a valid signature of its own.
func TestAnotherWalletsSignatureIsRefused(t *testing.T) {
	t.Parallel()

	mine, theirs := newWallet(t), newWallet(t)
	message := "sign in please"

	err := identity.VerifySignature(theirs.address, message, mine.sign(t, message))
	require.ErrorIs(t, err, apperr.ErrValidation)
}

func TestBothRecoveryIdEncodingsAreAccepted(t *testing.T) {
	t.Parallel()

	w := newWallet(t)
	message := "sign in please"
	signature := w.sign(t, message)

	raw, err := hex.DecodeString(strings.TrimPrefix(signature, "0x"))
	require.NoError(t, err)

	// Some wallets report the recovery id as 0 or 1 rather than 27 or 28. Both must work,
	// or a valid wallet is locked out over a formatting detail.
	legacy := append([]byte(nil), raw...)
	if legacy[64] >= 27 {
		legacy[64] -= 27
	}

	require.NoError(t, identity.VerifySignature(w.address, message, "0x"+hex.EncodeToString(legacy)))
}

func TestMalformedSignaturesAreRejected(t *testing.T) {
	t.Parallel()

	w := newWallet(t)
	message := "sign in please"
	valid := w.sign(t, message)

	raw, err := hex.DecodeString(strings.TrimPrefix(valid, "0x"))
	require.NoError(t, err)

	badRecovery := append([]byte(nil), raw...)
	badRecovery[64] = 9

	tampered := append([]byte(nil), raw...)
	tampered[10] ^= 0xff

	tests := []struct {
		name      string
		signature string
	}{
		{name: "empty", signature: ""},
		{name: "not hex", signature: "0xnothexatall"},
		{name: "too short", signature: "0xdeadbeef"},
		{name: "too long", signature: valid + "00"},
		{name: "impossible recovery id", signature: "0x" + hex.EncodeToString(badRecovery)},
		{name: "tampered body", signature: "0x" + hex.EncodeToString(tampered)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := identity.VerifySignature(w.address, message, tc.signature)
			require.ErrorIs(t, err, apperr.ErrValidation)
		})
	}
}

// TestFailureDoesNotNameTheRecoveredWallet keeps a probe from learning whose signature it is
// holding.
func TestFailureDoesNotNameTheRecoveredWallet(t *testing.T) {
	t.Parallel()

	mine, theirs := newWallet(t), newWallet(t)
	message := "sign in please"

	err := identity.VerifySignature(theirs.address, message, mine.sign(t, message))
	require.Error(t, err)
	assert.NotContains(t, strings.ToLower(err.Error()), strings.ToLower(mine.address[2:]))
}

func TestNormalizeWallet(t *testing.T) {
	t.Parallel()

	normalized, err := identity.NormalizeWallet("  0xAbC1230000000000000000000000000000000000  ")
	require.NoError(t, err)
	assert.Equal(t, "0xabc1230000000000000000000000000000000000", normalized)

	for _, invalid := range []string{"", "abc", "0x123", "0xZZ", strings.Repeat("0x00", 30)} {
		_, err := identity.NormalizeWallet(invalid)
		require.ErrorIsf(t, err, apperr.ErrValidation, "input %q", invalid)
	}
}
