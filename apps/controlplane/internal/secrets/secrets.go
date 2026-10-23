package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const version = "v1:"

type Manager struct {
	key []byte
}

func New(key []byte) (*Manager, error) {
	if len(key) != 32 {
		return nil, errors.New("secrets: key must be 32 bytes")
	}
	return &Manager{key: append([]byte(nil), key...)}, nil
}

func (m *Manager) Seal(plaintext, aad string) (string, error) {
	gcm, err := m.gcm()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secrets: generate nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
	return version + base64.StdEncoding.EncodeToString(sealed), nil
}

func (m *Manager) Open(sealed, aad string) (string, error) {
	if !strings.HasPrefix(sealed, version) {
		return "", errors.New("secrets: unsupported ciphertext version")
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, version))
	if err != nil {
		return "", fmt.Errorf("secrets: decode ciphertext: %w", err)
	}
	gcm, err := m.gcm()
	if err != nil {
		return "", err
	}
	if len(payload) < gcm.NonceSize()+gcm.Overhead() {
		return "", errors.New("secrets: ciphertext is too short")
	}
	nonce, ciphertext := payload[:gcm.NonceSize()], payload[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(aad))
	if err != nil {
		return "", fmt.Errorf("secrets: authenticate ciphertext: %w", err)
	}
	return string(plaintext), nil
}

func (m *Manager) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(m.key)
	if err != nil {
		return nil, fmt.Errorf("secrets: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: create GCM: %w", err)
	}
	return gcm, nil
}

// Load uses an environment key when configured; otherwise it reads or creates
// a private key file. The returned path is empty when the key came from env.
func Load(envKey, keyFile, dbURL string) (*Manager, string, error) {
	if envKey != "" {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(envKey))
		if err != nil {
			return nil, "", fmt.Errorf("secrets: VARDE_SECRETS_KEY must be base64: %w", err)
		}
		manager, err := New(key)
		if err != nil {
			return nil, "", fmt.Errorf("secrets: VARDE_SECRETS_KEY: %w", err)
		}
		return manager, "", nil
	}

	if keyFile == "" {
		keyFile = defaultKeyPath(dbURL)
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return nil, "", fmt.Errorf("secrets: create key directory: %w", err)
	}
	key, err := os.ReadFile(keyFile)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, "", fmt.Errorf("secrets: generate key: %w", err)
		}
		err = writeKey(keyFile, key)
		if errors.Is(err, os.ErrExist) {
			key, err = os.ReadFile(keyFile)
		}
	}
	if err != nil {
		return nil, "", fmt.Errorf("secrets: read key file: %w", err)
	}
	if err := os.Chmod(keyFile, 0o600); err != nil {
		return nil, "", fmt.Errorf("secrets: set key file permissions: %w", err)
	}
	manager, err := New(key)
	if err != nil {
		return nil, "", fmt.Errorf("secrets: key file must contain 32 raw bytes: %w", err)
	}
	return manager, keyFile, nil
}

func writeKey(path string, key []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func defaultKeyPath(dbURL string) string {
	if strings.HasPrefix(dbURL, "sqlite://") {
		dbPath := strings.TrimPrefix(dbURL, "sqlite://")
		if i := strings.IndexByte(dbPath, '?'); i >= 0 {
			dbPath = dbPath[:i]
		}
		if len(dbPath) >= 3 && dbPath[0] == '/' && dbPath[2] == ':' {
			dbPath = dbPath[1:]
		}
		if dbPath != "" {
			return filepath.Join(filepath.Dir(dbPath), "secrets.key")
		}
	}
	if runtime.GOOS == "windows" {
		if programData := os.Getenv("ProgramData"); programData != "" {
			return filepath.Join(programData, "VardeCP", "secrets.key")
		}
	}
	return "/var/lib/varde-cp/secrets.key"
}
