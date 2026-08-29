package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
)

// DecryptDEK decrypts a base64 encoded cipher text using the master key.
// The base64Cipher should be composed of IV (12 bytes) + CipherText.
func DecryptDEK(base64Cipher string, masterKey []byte) ([]byte, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes")
	}

	data, err := base64.StdEncoding.DecodeString(base64Cipher)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64 cipher: %w", err)
	}

	if len(data) < 12 {
		return nil, fmt.Errorf("cipher text is too short, missing IV")
	}

	iv := data[:12]
	cipherText := data[12:]

	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM mode: %w", err)
	}

	plainDEK, err := aesgcm.Open(nil, iv, cipherText, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt DEK: %w", err)
	}

	return plainDEK, nil
}
