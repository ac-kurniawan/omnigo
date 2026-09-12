package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"

	"github.com/ac-kurniawan/omnigo/internal/vault"
)

const prefixLen = 8 // "ak-" + 8 chars

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func GenerateKey() (raw, hash, prefix string, err error) {
	body, err := randHex(24)
	if err != nil {
		return "", "", "", err
	}
	raw = "ak-" + body
	hash = HashKey(raw)
	prefix = raw[:3+prefixLen]
	return raw, hash, prefix, nil
}

func IndexKeys(keys []vault.ClientKey) map[string]vault.ClientKey {
	index := make(map[string]vault.ClientKey, len(keys))
	for _, key := range keys {
		if key.Active && len(key.KeyHash) == sha256.Size*2 {
			index[key.KeyHash] = key
		}
	}
	return index
}

func Lookup(keys map[string]vault.ClientKey, raw string) (vault.ClientKey, bool) {
	key, ok := keys[HashKey(raw)]
	return key, ok
}
