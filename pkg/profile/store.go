package profile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Store defines persistence operations for encrypted user profiles.
type Store interface {
	Save(ctx context.Context, p *Profile) error
	Load(ctx context.Context) (*Profile, error)
	HasProfile(ctx context.Context) (bool, error)
	Delete(ctx context.Context) error
	Path() string
}

// FileStore implements atomic, encrypted disk persistence for profiles.
type FileStore struct {
	mu           sync.RWMutex
	path         string
	key          []byte
	kdfAlgorithm string // e.g. KDFArgon2id, KDFPBKDF2, or KDFRawKey
}

// NewFileStore creates a new FileStore using a 32-byte raw key or passphrase.
func NewFileStore(path string, key []byte) *FileStore {
	var algo string
	if len(key) == 32 {
		algo = KDFRawKey
	} else {
		algo = KDFArgon2id
	}
	return &FileStore{
		path:         path,
		key:          key,
		kdfAlgorithm: algo,
	}
}

// NewFileStoreWithKDF creates a FileStore with an explicit KDF preference.
func NewFileStoreWithKDF(path string, key []byte, algo string) *FileStore {
	return &FileStore{
		path:         path,
		key:          key,
		kdfAlgorithm: algo,
	}
}

// Path returns the configured filesystem path for the profile.
func (s *FileStore) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// SetKey updates the encryption key or passphrase in memory.
func (s *FileStore) SetKey(key []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.key = key
	if len(key) == 32 {
		s.kdfAlgorithm = KDFRawKey
	} else {
		s.kdfAlgorithm = KDFArgon2id
	}
}

// HasProfile checks whether an encrypted profile exists on disk.
func (s *FileStore) HasProfile(ctx context.Context) (bool, error) {
	s.mu.RLock()
	filePath := s.path
	s.mu.RUnlock()

	if filePath == "" {
		return false, nil
	}

	fi, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	return fi.Size() > 0, nil
}

// Save atomically encrypts and persists the profile to disk.
func (s *FileStore) Save(ctx context.Context, p *Profile) error {
	if p == nil {
		return ErrCorruptedProfile
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.key) == 0 {
		return ErrEmptyPassphrase
	}

	plaintext, err := encodeInternal(p)
	if err != nil {
		return fmt.Errorf("failed to encode profile: %w", err)
	}

	// Determine KDF parameters and derive the 32-byte AES key
	var kdfParams KDFParams
	var aesKey []byte

	if s.kdfAlgorithm == KDFRawKey && len(s.key) == 32 {
		kdfParams = KDFParams{
			Algorithm: KDFRawKey,
			KeyLength: 32,
		}
		aesKey = s.key
	} else {
		salt, err := GenerateSalt(16)
		if err != nil {
			return err
		}
		if s.kdfAlgorithm == KDFPBKDF2 {
			kdfParams = DefaultPBKDF2Params(salt)
		} else {
			kdfParams = DefaultArgon2idParams(salt)
		}
		derived, err := DeriveKey(s.key, kdfParams)
		if err != nil {
			return err
		}
		aesKey = derived
	}

	env, err := EncryptPayload(plaintext, aesKey, kdfParams)
	if err != nil {
		return fmt.Errorf("failed to encrypt profile: %w", err)
	}

	envBytes, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal envelope: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create profile directory: %w", err)
	}

	// Atomic write via temporary file in the same directory
	tmpFile, err := os.CreateTemp(dir, ".profile-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temporary profile file: %w", err)
	}
	tmpName := tmpFile.Name()
	defer func() {
		// Clean up temporary file if rename was not reached
		_ = os.Remove(tmpName)
	}()

	_ = os.Chmod(tmpName, 0600)

	if _, err := tmpFile.Write(envBytes); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to write to temporary profile file: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to flush temporary profile file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temporary profile file: %w", err)
	}

	// Atomic rename over target path
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("failed to atomically replace profile file: %w", err)
	}

	// Sync parent directory if supported
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}

	return nil
}

// Load loads and decrypts the profile from disk into memory.
func (s *FileStore) Load(ctx context.Context) (*Profile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.key) == 0 {
		return nil, ErrEmptyPassphrase
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrProfileNotFound
		}
		return nil, err
	}

	var env EncryptedEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%w: invalid envelope JSON: %v", ErrCorruptedProfile, err)
	}

	var aesKey []byte
	if env.KDF.Algorithm == KDFRawKey {
		if len(s.key) != 32 {
			return nil, ErrInvalidKeyLength
		}
		aesKey = s.key
	} else {
		derived, err := DeriveKey(s.key, env.KDF)
		if err != nil {
			return nil, fmt.Errorf("%w: key derivation failed: %v", ErrDecryptionFailed, err)
		}
		aesKey = derived
	}

	plaintext, err := DecryptPayload(&env, aesKey)
	if err != nil {
		return nil, err
	}

	p, err := decodeInternal(plaintext)
	if err != nil {
		return nil, err
	}

	return p, nil
}

// Delete removes the profile from disk.
func (s *FileStore) Delete(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// EnsureKeyFile ensures a local 32-byte master keyfile exists with restricted (0600) permissions.
// If the keyfile exists, it reads and validates it. If not, it generates a cryptographically random 32-byte key.
func EnsureKeyFile(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("keyfile path cannot be empty")
	}

	fi, err := os.Stat(path)
	if err == nil {
		// Verify file permissions on non-Windows platforms
		if runtime.GOOS != "windows" && fi.Mode().Perm()&0077 != 0 {
			// Automatically attempt to enforce 0600 if permissions are too permissive
			_ = os.Chmod(path, 0600)
			if updatedFi, err := os.Stat(path); err == nil && updatedFi.Mode().Perm()&0077 != 0 {
				return nil, fmt.Errorf("%w: keyfile %s has mode %04o", ErrInsecurePermissions, path, fi.Mode().Perm())
			}
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read keyfile %s: %w", path, err)
		}

		trimmed := strings.TrimSpace(string(content))
		if len(trimmed) == 64 {
			if raw, err := hex.DecodeString(trimmed); err == nil && len(raw) == 32 {
				return raw, nil
			}
		}
		if len(content) == 32 {
			return content, nil
		}
		if len(trimmed) > 0 {
			return []byte(trimmed), nil
		}
		return nil, fmt.Errorf("%w: keyfile %s is empty", ErrCorruptedProfile, path)
	}

	if !os.IsNotExist(err) {
		return nil, err
	}

	// Generate a new 32-byte cryptographically secure random key
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("failed to generate random key: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create keyfile directory %s: %w", dir, err)
	}

	hexKey := hex.EncodeToString(key) + "\n"
	tmpFile, err := os.CreateTemp(dir, ".key-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary keyfile: %w", err)
	}
	tmpName := tmpFile.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	_ = os.Chmod(tmpName, 0600)

	if _, err := tmpFile.WriteString(hexKey); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("failed to write keyfile: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("failed to sync keyfile: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return nil, err
	}

	if err := os.Rename(tmpName, path); err != nil {
		return nil, fmt.Errorf("failed to atomically place keyfile %s: %w", path, err)
	}

	return key, nil
}

// ErrKeyRecoveryRequired means existing key material must be restored, not
// replaced. Generating a different key would destroy access to the profile.
var ErrKeyRecoveryRequired = errors.New("profile key recovery required")

func readProtectedKey(path string) ([]byte, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(blob) == 0 {
		return nil, fmt.Errorf("%w: protected key is empty; restore the original key file", ErrKeyRecoveryRequired)
	}
	key, err := UnprotectKeyDPAPI(blob)
	if err != nil || len(key) == 0 {
		return nil, fmt.Errorf("%w: cannot decrypt the protected key; use the original Windows user or restore its key backup: %v", ErrKeyRecoveryRequired, err)
	}
	// Legacy files may contain a passphrase rather than a raw 32-byte key.
	return key, nil
}

// writeNewProtectedKey publishes a complete file without replacing any existing
// key, including one created concurrently by another process.
func writeNewProtectedKey(path string, blob []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".protected-key-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(blob); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// Linking is atomic and fails if path already exists; Rename may overwrite.
	if err := os.Link(tmp, path); err != nil {
		return fmt.Errorf("%w: could not publish protected key without replacing an existing file: %w", ErrKeyRecoveryRequired, err)
	}
	return nil
}

// ResolveOrEnsureProtectedKey resolves or generates a master key file.
// On Windows, if DPAPI is available, it stores the key in DPAPI-encrypted format
// (keyPath + ".dpapi"), migrating and deleting legacy plaintext hex files if found.
// On non-Windows platforms, it uses EnsureKeyFile (file with 0600 permissions).
func ResolveOrEnsureProtectedKey(keyPath string) ([]byte, error) {
	if runtime.GOOS != "windows" || !DPAPIAvailable() {
		return EnsureKeyFile(keyPath)
	}
	dpapiPath := keyPath + ".dpapi"
	key, err := readProtectedKey(dpapiPath)
	if err == nil {
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: existing protected key was left untouched: %w", ErrKeyRecoveryRequired, err)
	}

	raw, err := os.ReadFile(keyPath)
	if err == nil {
		trimmed := strings.TrimSpace(string(raw))
		if len(trimmed) == 0 && len(raw) != 32 {
			return nil, fmt.Errorf("%w: legacy key is empty; restore its backup", ErrKeyRecoveryRequired)
		}
		key = []byte(trimmed)
		if len(raw) == 32 {
			key = raw
		}
		if len(trimmed) == 64 {
			if decoded, err := hex.DecodeString(trimmed); err == nil {
				key = decoded
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: cannot read existing legacy key: %w", ErrKeyRecoveryRequired, err)
	} else {
		key = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("failed to generate random key: %w", err)
		}
	}
	legacy := err == nil
	blob, err := ProtectKeyDPAPI(key)
	if err != nil {
		return nil, fmt.Errorf("failed to DPAPI protect key: %w", err)
	}
	if err := writeNewProtectedKey(dpapiPath, blob); err != nil {
		return nil, err
	}
	if legacy {
		if err := os.Remove(keyPath); err != nil {
			return nil, fmt.Errorf("protected key saved, but cannot remove legacy plaintext key: %w", err)
		}
	}
	return key, nil
}

// ResolveKey resolves the profile encryption key using precedence:
// 1. Direct key/passphrase argument or flag
// 2. Environment variable BOMBECAM_PROFILE_KEY
// 3. Keyfile at keyFilePath (or env BOMBECAM_PROFILE_KEY_FILE)
// 4. Auto-generated keyfile at keyFilePath if specified
func ResolveKey(directKey, keyFilePath string) ([]byte, error) {
	if directKey != "" {
		return decodeKey(directKey), nil
	}

	if envKey := os.Getenv("BOMBECAM_PROFILE_KEY"); envKey != "" {
		return decodeKey(envKey), nil
	}

	resolvedKeyFile := keyFilePath
	if resolvedKeyFile == "" {
		resolvedKeyFile = os.Getenv("BOMBECAM_PROFILE_KEY_FILE")
	}

	if resolvedKeyFile != "" {
		return ResolveOrEnsureProtectedKey(resolvedKeyFile)
	}

	return nil, nil
}

// decodeKey reads a key given as 64 hex characters or as raw bytes.
func decodeKey(key string) []byte {
	trimmed := strings.TrimSpace(key)
	if len(trimmed) == 64 {
		if raw, err := hex.DecodeString(trimmed); err == nil && len(raw) == 32 {
			return raw
		}
	}
	return []byte(trimmed)
}
