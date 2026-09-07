// Package wallettest is a software wallet for tests.
//
// It exists so authentication is exercised with real signatures rather than a stub that
// returns true: a test that fakes the signature proves nothing about the one property
// login rests on, which is that only the holder of a private key can produce one.
package wallettest

import (
	"encoding/hex"
	"strconv"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
)

// Wallet is a generated key and the address derived from it.
type Wallet struct {
	Key *secp256k1.PrivateKey
	// Address is the 0x-prefixed EVM address, the way a wallet reports it.
	Address string
}

// New generates a wallet.
func New(t *testing.T) *Wallet {
	t.Helper()

	key, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// An EVM address is the last 20 bytes of the keccak digest of the uncompressed public
	// key, minus its leading format byte.
	uncompressed := key.PubKey().SerializeUncompressed()
	digest := keccak(uncompressed[1:])
	return &Wallet{Key: key, Address: "0x" + hex.EncodeToString(digest[12:])}
}

// Sign produces the 65-byte r||s||v signature a wallet returns from personal_sign.
func (w *Wallet) Sign(t *testing.T, message string) string {
	t.Helper()

	prefixed := "\x19Ethereum Signed Message:\n" + strconv.Itoa(len(message)) + message
	compact := ecdsa.SignCompact(w.Key, keccak([]byte(prefixed)), false)

	// SignCompact puts the recovery byte first; Ethereum puts it last.
	signature := make([]byte, 65)
	copy(signature, compact[1:])
	signature[64] = compact[0]
	return "0x" + hex.EncodeToString(signature)
}

func keccak(data []byte) []byte {
	hash := sha3.NewLegacyKeccak256()
	hash.Write(data)
	return hash.Sum(nil)
}
