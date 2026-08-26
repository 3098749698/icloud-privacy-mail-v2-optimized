package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

const appleCredentialCipherPrefix = "v1:"

type AppleCredentialSecret struct {
	AccountID       string
	AppleID         string
	Password        string
	TwoFactorMethod string
}

func (s *Store) SaveAppleCredential(accountID, appleID, password, twoFactorMethod string) error {
	accountID = strings.TrimSpace(accountID)
	appleID = strings.ToLower(strings.TrimSpace(appleID))
	if accountID == "" || appleID == "" || strings.TrimSpace(password) == "" {
		return errors.New("保存 Apple 登录凭据所需字段不完整")
	}
	key, err := s.appleCredentialKey()
	if err != nil {
		return err
	}
	ciphertext, err := encryptAppleCredential(key, accountID, password)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	accountExists := false
	for _, account := range s.state.AppleAccounts {
		if account.ID == accountID {
			accountExists = true
			if appleID == "" {
				appleID = strings.ToLower(strings.TrimSpace(account.AppleID))
			}
			break
		}
	}
	if !accountExists {
		return errors.New("Apple 账号不存在")
	}
	now := time.Now()
	for index := range s.state.AppleCredentials {
		if s.state.AppleCredentials[index].AccountID != accountID {
			continue
		}
		createdAt := s.state.AppleCredentials[index].CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		s.state.AppleCredentials[index] = domain.AppleCredential{
			AccountID:          accountID,
			AppleID:            appleID,
			PasswordCiphertext: ciphertext,
			TwoFactorMethod:    strings.TrimSpace(twoFactorMethod),
			CreatedAt:          createdAt,
			UpdatedAt:          now,
		}
		return s.saveLocked()
	}
	s.state.AppleCredentials = append(s.state.AppleCredentials, domain.AppleCredential{
		AccountID:          accountID,
		AppleID:            appleID,
		PasswordCiphertext: ciphertext,
		TwoFactorMethod:    strings.TrimSpace(twoFactorMethod),
		CreatedAt:          now,
		UpdatedAt:          now,
	})
	return s.saveLocked()
}

func (s *Store) AppleCredential(accountID string) (AppleCredentialSecret, bool, error) {
	accountID = strings.TrimSpace(accountID)
	s.mu.RLock()
	var encrypted domain.AppleCredential
	found := false
	for _, credential := range s.state.AppleCredentials {
		if credential.AccountID == accountID {
			encrypted = credential
			found = true
			break
		}
	}
	s.mu.RUnlock()
	if !found {
		return AppleCredentialSecret{}, false, nil
	}
	key, err := s.appleCredentialKey()
	if err != nil {
		return AppleCredentialSecret{}, false, err
	}
	password, err := decryptAppleCredential(key, accountID, encrypted.PasswordCiphertext)
	if err != nil {
		return AppleCredentialSecret{}, false, err
	}
	return AppleCredentialSecret{
		AccountID:       accountID,
		AppleID:         encrypted.AppleID,
		Password:        password,
		TwoFactorMethod: encrypted.TwoFactorMethod,
	}, true, nil
}

func (s *Store) HasAppleCredential(accountID string) bool {
	accountID = strings.TrimSpace(accountID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, credential := range s.state.AppleCredentials {
		if credential.AccountID == accountID && strings.TrimSpace(credential.PasswordCiphertext) != "" {
			return true
		}
	}
	return false
}

func (s *Store) appleCredentialKey() ([]byte, error) {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	if len(s.credentialKey) == 32 {
		return append([]byte(nil), s.credentialKey...), nil
	}
	keyPath := s.path + ".apple-credentials.key"
	data, err := os.ReadFile(keyPath)
	if err == nil {
		key, parseErr := parseAppleCredentialKey(data)
		if parseErr != nil {
			return nil, fmt.Errorf("读取 Apple 凭据密钥失败：%w", parseErr)
		}
		s.credentialKey = key
		return append([]byte(nil), key...), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("读取 Apple 凭据密钥失败：%w", err)
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := writeSecretFile(keyPath, []byte(base64.RawStdEncoding.EncodeToString(key)+"\n")); err != nil {
		return nil, fmt.Errorf("保存 Apple 凭据密钥失败：%w", err)
	}
	s.credentialKey = key
	return append([]byte(nil), key...), nil
}

func parseAppleCredentialKey(data []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(data))
	if decoded, err := base64.RawStdEncoding.DecodeString(trimmed); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if len(data) == 32 {
		return append([]byte(nil), data...), nil
	}
	return nil, errors.New("密钥格式不是 32 字节或有效 Base64")
}

func writeSecretFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credential-key-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func encryptAppleCredential(key []byte, accountID, password string) (string, error) {
	aead, err := appleCredentialAEAD(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(password), []byte(accountID))
	return appleCredentialCipherPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func decryptAppleCredential(key []byte, accountID, ciphertext string) (string, error) {
	if !strings.HasPrefix(ciphertext, appleCredentialCipherPrefix) {
		return "", errors.New("Apple 凭据密文版本不受支持")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ciphertext, appleCredentialCipherPrefix))
	if err != nil {
		return "", errors.New("Apple 凭据密文格式错误")
	}
	aead, err := appleCredentialAEAD(key)
	if err != nil {
		return "", err
	}
	if len(sealed) < aead.NonceSize() {
		return "", errors.New("Apple 凭据密文长度错误")
	}
	nonce, payload := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, payload, []byte(accountID))
	if err != nil {
		return "", errors.New("Apple 凭据解密失败，请检查 Apple 凭据密钥文件")
	}
	return string(plain), nil
}

func appleCredentialAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("Apple 凭据密钥长度错误")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
