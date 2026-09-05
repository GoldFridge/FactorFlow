package marketdata

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Field and record separators for canonical serialization. Using control characters that
// cannot appear in an identifier or a decimal literal keeps the encoding unambiguous
// without the cost and ordering pitfalls of JSON.
const (
	fieldSep  = "\x1f"
	recordSep = "\x1e"
)

// hashQuery digests the request that produced a snapshot: provider, network, asset, query
// text and variables in sorted order.
func hashQuery(q Query) string {
	var b strings.Builder
	b.WriteString(q.Provider)
	b.WriteString(fieldSep)
	b.WriteString(q.Network)
	b.WriteString(fieldSep)
	b.WriteString(q.Asset)
	b.WriteString(fieldSep)
	b.WriteString(normalizeGraphQL(q.GraphQL))

	names := make([]string, 0, len(q.Variables))
	for name := range q.Variables {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		b.WriteString(recordSep)
		b.WriteString(name)
		b.WriteString(fieldSep)
		b.WriteString(q.Variables[name])
	}

	return digest(b.String())
}

// hashPayload digests the market rows a snapshot was built from, together with the query
// and the observation time.
//
// Only inputs are hashed: the benchmark, premium and volatility are recomputed from these
// rows by published code, so hashing them too would add nothing a verifier could check
// independently.
func hashPayload(s *Snapshot) string {
	var b strings.Builder
	b.WriteString(s.QueryHash)
	b.WriteString(fieldSep)
	b.WriteString(s.ObservedAt.UTC().Format(time.RFC3339Nano))

	for _, m := range s.Markets {
		b.WriteString(recordSep)
		b.WriteString(m.SubgraphID)
		b.WriteString(fieldSep)
		b.WriteString(m.ID)
		b.WriteString(fieldSep)
		b.WriteString(m.Asset)
		b.WriteString(fieldSep)
		b.WriteString(m.NetSupplyAPY.StringFixed(rateScale))
		b.WriteString(fieldSep)
		b.WriteString(strconv.FormatInt(m.AvailableLiquidity.Minor(), 10))
		b.WriteString(fieldSep)
		b.WriteString(m.AvailableLiquidity.Currency().String())
		b.WriteString(fieldSep)
		b.WriteString(strconv.FormatInt(m.BlockNumber, 10))
	}

	return digest(b.String())
}

// digest returns the 0x-prefixed lowercase SHA-256 of s, the digest format the assessment
// and audit records expect.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "0x" + hex.EncodeToString(sum[:])
}

// normalizeGraphQL collapses insignificant whitespace so that reindenting a query does not
// invalidate every price that referenced it.
func normalizeGraphQL(query string) string {
	return strings.Join(strings.Fields(query), " ")
}
