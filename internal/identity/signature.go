// Package identity authenticates a caller by their wallet.
//
// There are no passwords and no server-held keys. A wallet proves who it is by signing a
// one-time challenge, and the server checks the signature recovers to the address that
// asked. That is the whole mechanism: the specification puts custody of user keys out of
// scope, and this keeps it there.
package identity

import (
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

// signatureLen is the length of an Ethereum signature: 32 bytes of r, 32 of s, one of v.
const signatureLen = 65

// personalSignPrefix is the EIP-191 prefix a wallet applies before hashing a message. It
// exists so that a signature over a human-readable message can never be replayed as a
// signature over a transaction.
const personalSignPrefix = "\x19Ethereum Signed Message:\n"

// RecoverAddress returns the wallet address that produced an EIP-191 personal_sign
// signature over message.
//
// The signature is the 65-byte r||s||v form that every EVM wallet returns from
// personal_sign, hex encoded with or without the 0x prefix.
func RecoverAddress(message string, signature string) (string, error) {
	raw, err := decodeSignature(signature)
	if err != nil {
		return "", err
	}

	// The recovery id arrives as 27 or 28 from most wallets and as 0 or 1 from a few, and
	// both are in the wild. Normalizing here rather than rejecting one form keeps a valid
	// wallet from being unable to sign in over a formatting detail.
	recoveryID := raw[64]
	if recoveryID >= 27 {
		recoveryID -= 27
	}
	if recoveryID > 1 {
		return "", apperr.Invalid("signature", "recovery id %d is not valid", raw[64])
	}

	// decred's RecoverCompact expects the recovery byte first and the header offset by 27,
	// where Ethereum puts it last.
	compact := make([]byte, signatureLen)
	compact[0] = recoveryID + 27
	copy(compact[1:], raw[:64])

	publicKey, _, err := ecdsa.RecoverCompact(compact, personalSignHash(message))
	if err != nil {
		return "", apperr.Invalid("signature", "does not recover to a public key")
	}

	// An Ethereum address is the last 20 bytes of the keccak-256 of the uncompressed public
	// key without its 0x04 tag.
	uncompressed := publicKey.SerializeUncompressed()
	digest := keccak256(uncompressed[1:])
	return "0x" + hex.EncodeToString(digest[12:]), nil
}

// VerifySignature checks that a signature over message was produced by wallet.
func VerifySignature(wallet, message, signature string) error {
	recovered, err := RecoverAddress(message, signature)
	if err != nil {
		return err
	}
	if !strings.EqualFold(recovered, strings.TrimSpace(wallet)) {
		// The recovered address is not reported back: telling a caller which wallet their
		// signature belongs to helps nobody but someone probing with borrowed signatures.
		return apperr.Invalid("signature", "was not produced by wallet %s", wallet)
	}
	return nil
}

// personalSignHash computes the digest a wallet actually signs under EIP-191.
func personalSignHash(message string) []byte {
	prefixed := personalSignPrefix + strconv.Itoa(len(message)) + message
	return keccak256([]byte(prefixed))
}

func keccak256(data []byte) []byte {
	hash := sha3.NewLegacyKeccak256()
	hash.Write(data)
	return hash.Sum(nil)
}

func decodeSignature(signature string) ([]byte, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(signature), "0x")

	raw, err := hex.DecodeString(trimmed)
	if err != nil {
		return nil, apperr.Invalid("signature", "must be hex encoded")
	}
	if len(raw) != signatureLen {
		return nil, apperr.Invalid("signature", "must be %d bytes, got %d", signatureLen, len(raw))
	}
	return raw, nil
}

// NormalizeWallet lowercases an address after checking its shape, so a wallet is one string
// wherever it is stored or compared.
func NormalizeWallet(wallet string) (string, error) {
	trimmed := strings.TrimSpace(wallet)
	if !walletAddress.MatchString(trimmed) {
		return "", apperr.Invalid("wallet", "must be a 0x-prefixed 20-byte address")
	}
	return strings.ToLower(trimmed), nil
}

// describeAddress renders an address for a message, keeping the checksum-free lowercase
// form the rest of the system stores.
func describeAddress(wallet string) string { return strings.ToLower(strings.TrimSpace(wallet)) }
