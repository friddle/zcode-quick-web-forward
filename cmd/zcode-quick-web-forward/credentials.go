package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// zcodeUserInfo mirrors the runtime's oauth user_info blob (decrypted from
// ~/.zcode/v2/credentials.json, key "oauth:zai:user_info").
type zcodeUserInfo struct {
	Name   string `json:"name"`
	Email  string `json:"email,omitempty"`
	Avatar string `json:"avatar,omitempty"`
	UserID string `json:"user_id,omitempty"`
}

// displayUsername picks the phone UI's username field (username/displayName).
func (u *zcodeUserInfo) displayUsername() string {
	if u == nil {
		return ""
	}
	return u.Name
}

// decryptZCodeCredential reverses the runtime's enc:v1 credential format:
//
//	enc:v1:<iv>.<tag>.<ciphertext>   (base64url parts)
//	key = sha256(secret)
//	secret = $ZCODE_CREDENTIAL_SECRET or
//	         "zcode-credential-fallback:<platform>:<homedir>:<username>"
//
// (reverse-engineered from glm/zcode.cjs createZCodeCredentialCipher)
func decryptZCodeCredential(value string) (string, bool) {
	const prefix = "enc:v1:"
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	parts := strings.Split(value[len(prefix):], ".")
	if len(parts) != 3 {
		return "", false
	}
	iv, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	tag, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	ct, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || len(iv) != 12 || len(tag) != 16 {
		return "", false
	}
	secret := os.Getenv("ZCODE_CREDENTIAL_SECRET")
	if secret == "" {
		home, _ := os.UserHomeDir()
		user := "unknown"
		if u, err := userCurrentName(); err == nil {
			user = u
		}
		secret = "zcode-credential-fallback:" + platformKey() + ":" + home + ":" + user
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", false
	}
	plain, err := gcm.Open(nil, iv, ct, tag)
	if err != nil {
		return "", false
	}
	return string(plain), true
}

// loadZcodeUserInfo decrypts the signed-in account info, if any.
func loadZcodeUserInfo() *zcodeUserInfo {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(home, ".zcode", "v2", "credentials.json"))
	if err != nil {
		return nil
	}
	var creds map[string]string
	if json.Unmarshal(b, &creds) != nil {
		return nil
	}
	raw, ok := creds["oauth:zai:user_info"]
	if !ok {
		return nil
	}
	plain, ok := decryptZCodeCredential(raw)
	if !ok {
		return nil
	}
	var ui zcodeUserInfo
	if json.Unmarshal([]byte(plain), &ui) != nil || ui.Name == "" {
		return nil
	}
	return &ui
}
