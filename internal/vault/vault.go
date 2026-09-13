package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type ProviderSecret struct {
	APIKey       string    `yaml:"api_key,omitempty"`
	AccessToken  string    `yaml:"access_token,omitempty"`
	RefreshToken string    `yaml:"refresh_token,omitempty"`
	IDToken      string    `yaml:"id_token,omitempty"`
	ExpiresAt    time.Time `yaml:"expires_at,omitempty"`
	ProjectID    string    `yaml:"project_id,omitempty"`
	AccountID    string    `yaml:"account_id,omitempty"`
	Email        string    `yaml:"email,omitempty"`
}

type ClientKey struct {
	ID        string    `yaml:"id"`
	Name      string    `yaml:"name"`
	KeyHash   string    `yaml:"key_hash"`
	Prefix    string    `yaml:"prefix"`
	CreatedAt time.Time `yaml:"created_at"`
	Active    bool      `yaml:"active"`
}

type Vault struct {
	ProviderSecrets map[string]ProviderSecret `yaml:"provider_secrets"`
	ClientKeys      []ClientKey               `yaml:"client_keys"`
}

type file struct {
	Version    int    `yaml:"version"`
	Nonce      []byte `yaml:"nonce"`
	Ciphertext []byte `yaml:"ciphertext"`
}

func encrypt(key, plaintext []byte) (nonce, ciphertext []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, gcm.Seal(nil, nonce, plaintext, nil), nil
}

func decrypt(key, nonce, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func Save(path string, key []byte, v *Vault) error {
	plaintext, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal vault: %w", err)
	}
	nonce, ciphertext, err := encrypt(key, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt vault: %w", err)
	}
	out, err := yaml.Marshal(file{Version: 1, Nonce: nonce, Ciphertext: ciphertext})
	if err != nil {
		return fmt.Errorf("marshal file: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write vault: %w", err)
	}
	return nil
}

func Load(path string, key []byte) (*Vault, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read vault: %w", err)
	}
	var f file
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse vault: %w", err)
	}
	plaintext, err := decrypt(key, f.Nonce, f.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt vault: %w", err)
	}
	var v Vault
	if err := yaml.Unmarshal(plaintext, &v); err != nil {
		return nil, fmt.Errorf("unmarshal vault: %w", err)
	}
	return &v, nil
}

func ResolveKey(envKey, keyPath string) ([]byte, error) {
	if envKey == "" {
		envKey = os.Getenv("OMNIGO_SECRET_KEY")
		if envKey == "" {
			envKey = os.Getenv("AIGO_SECRET_KEY")
		}
	}
	if envKey != "" {
		if len(envKey) != 32 {
			return nil, fmt.Errorf("OMNIGO_SECRET_KEY must be exactly 32 bytes")
		}
		return []byte(envKey), nil
	}
	if b, err := os.ReadFile(keyPath); err == nil {
		if len(b) != 32 {
			return nil, fmt.Errorf("key file %s must be exactly 32 bytes", keyPath)
		}
		return b, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("write key file: %w", err)
	}
	return key, nil
}
