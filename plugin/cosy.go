package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	cosyVersion          = "1.0.1"
	cosyClientType       = "5"
	cosyMachineType      = "5"
	cosyMachineOS        = "x86_64_windows"
	cosyDataPolicy       = "disagree"
	cosyLoginVersion     = "v2"
	qoderStandardBase64  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	qoderCustomBase64    = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	qoderRSAPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`
)

var (
	processMachineIDOnce sync.Once
	processMachineID     string
)

type cosyUserInfo struct {
	UID                string `json:"uid"`
	SecurityOAuthToken string `json:"security_oauth_token"`
	Name               string `json:"name"`
	AID                string `json:"aid"`
	Email              string `json:"email"`
}

type cosyPayload struct {
	Version     string `json:"version"`
	RequestID   string `json:"requestId"`
	Info        string `json:"info"`
	CosyVersion string `json:"cosyVersion"`
	IDEVersion  string `json:"ideVersion"`
}

// encodeQoderBody applies Qoder's Encode=1 base64 permutation and alphabet.
func encodeQoderBody(plaintext []byte) []byte {
	standard := base64.StdEncoding.EncodeToString(plaintext)
	n := len(standard)
	third := n / 3
	rearranged := standard[n-third:] + standard[third:n-third] + standard[:third]
	encoded := make([]byte, len(rearranged))
	for i := range rearranged {
		character := rearranged[i]
		if character == '=' {
			encoded[i] = '$'
			continue
		}
		index := strings.IndexByte(qoderStandardBase64, character)
		if index < 0 {
			encoded[i] = character
			continue
		}
		encoded[i] = qoderCustomBase64[index]
	}
	return encoded
}

func cosySigPath(requestURL string) (string, error) {
	parsed, errParse := url.Parse(requestURL)
	if errParse != nil {
		return "", fmt.Errorf("cosy sigpath: invalid request URL: %w", errParse)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("cosy sigpath: request URL must be absolute")
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	path = strings.TrimPrefix(path, "/algo")
	if path == "" {
		path = "/"
	}
	return path, nil
}

func pkcs7Pad(plaintext []byte, blockSize int) []byte {
	paddingLength := blockSize - len(plaintext)%blockSize
	padded := make([]byte, len(plaintext)+paddingLength)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(paddingLength)
	}
	return padded
}

func aesEncryptCBCBase64(plaintext []byte, key []byte) (string, error) {
	block, errCipher := aes.NewCipher(key)
	if errCipher != nil {
		return "", fmt.Errorf("cosy AES key: %w", errCipher)
	}
	padded := pkcs7Pad(plaintext, block.BlockSize())
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(ciphertext, padded)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func parseQoderRSAPublicKey() (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(qoderRSAPublicKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("cosy RSA public key is invalid")
	}
	parsed, errParse := x509.ParsePKIXPublicKey(block.Bytes)
	if errParse != nil {
		return nil, fmt.Errorf("cosy RSA public key: %w", errParse)
	}
	publicKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("cosy public key is not RSA")
	}
	return publicKey, nil
}

func randomHex(length int) (string, error) {
	if length%2 != 0 {
		return "", fmt.Errorf("random hex length must be even")
	}
	random := make([]byte, length/2)
	if _, errRead := rand.Read(random); errRead != nil {
		return "", fmt.Errorf("read random bytes: %w", errRead)
	}
	return hex.EncodeToString(random), nil
}

func randomUUID() (string, error) {
	value := make([]byte, 16)
	if _, errRead := rand.Read(value); errRead != nil {
		return "", fmt.Errorf("read random UUID: %w", errRead)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" +
		encoded[16:20] + "-" + encoded[20:32], nil
}

func resolveMachineID(configured string) string {
	if machineID := strings.TrimSpace(configured); machineID != "" {
		return machineID
	}
	if home, errHome := os.UserHomeDir(); errHome == nil {
		contents, errRead := os.ReadFile(filepath.Join(home, ".qoder", ".auth", "machine_id"))
		if errRead == nil {
			if machineID := strings.TrimSpace(string(contents)); machineID != "" {
				return machineID
			}
		}
	}
	processMachineIDOnce.Do(func() {
		processMachineID, _ = randomUUID()
		if processMachineID == "" {
			processMachineID = fmt.Sprintf("qoder-%d", time.Now().UnixNano())
		}
	})
	return processMachineID
}

// buildCosyHeaders signs the exact wire body sent to the Qoder gateway.
func buildCosyHeaders(body []byte, requestURL string, creds qoderCredentials) (httpHeaders, error) {
	if strings.TrimSpace(creds.UserID) == "" {
		return nil, fmt.Errorf("cosy: user id is empty")
	}
	if strings.TrimSpace(creds.JobToken) == "" {
		return nil, fmt.Errorf("cosy: job token is empty")
	}
	sigPath, errPath := cosySigPath(requestURL)
	if errPath != nil {
		return nil, errPath
	}
	aesKeyText, errKey := randomHex(16)
	if errKey != nil {
		return nil, fmt.Errorf("cosy AES key: %w", errKey)
	}
	aesKey := []byte(aesKeyText)
	userInfo, errUserInfo := json.Marshal(cosyUserInfo{
		UID:                creds.UserID,
		SecurityOAuthToken: creds.JobToken,
		Name:               creds.Name,
		AID:                "",
		Email:              creds.Email,
	})
	if errUserInfo != nil {
		return nil, fmt.Errorf("cosy user info: %w", errUserInfo)
	}
	info, errEncrypt := aesEncryptCBCBase64(userInfo, aesKey)
	if errEncrypt != nil {
		return nil, errEncrypt
	}
	publicKey, errPublicKey := parseQoderRSAPublicKey()
	if errPublicKey != nil {
		return nil, errPublicKey
	}
	encryptedKey, errRSA := rsa.EncryptPKCS1v15(rand.Reader, publicKey, aesKey)
	if errRSA != nil {
		return nil, fmt.Errorf("cosy RSA encryption: %w", errRSA)
	}
	cosyKey := base64.StdEncoding.EncodeToString(encryptedKey)
	requestID, errRequestID := randomUUID()
	if errRequestID != nil {
		return nil, errRequestID
	}
	payloadJSON, errPayload := json.Marshal(cosyPayload{
		Version:     "v1",
		RequestID:   requestID,
		Info:        info,
		CosyVersion: cosyVersion,
		IDEVersion:  "",
	})
	if errPayload != nil {
		return nil, fmt.Errorf("cosy payload: %w", errPayload)
	}
	payload := base64.StdEncoding.EncodeToString(payloadJSON)
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	signatureInput := []byte(payload + "\n" + cosyKey + "\n" + timestamp + "\n")
	signatureInput = append(signatureInput, body...)
	signatureInput = append(signatureInput, '\n')
	signatureInput = append(signatureInput, sigPath...)
	signature := fmt.Sprintf("%x", md5.Sum(signatureInput))
	bodyHash := fmt.Sprintf("%x", md5.Sum(body))
	xRequestID, errXRequestID := randomUUID()
	if errXRequestID != nil {
		return nil, errXRequestID
	}
	machineID := resolveMachineID(creds.MachineID)
	return httpHeaders{
		"Authorization":          {"Bearer COSY." + payload + "." + signature},
		"Cosy-Key":               {cosyKey},
		"Cosy-User":              {creds.UserID},
		"Cosy-Date":              {timestamp},
		"Cosy-Version":           {cosyVersion},
		"Cosy-Machineid":         {machineID},
		"Cosy-Machinetoken":      {machineID},
		"Cosy-Machinetype":       {cosyMachineType},
		"Cosy-Machineos":         {cosyMachineOS},
		"Cosy-Clienttype":        {cosyClientType},
		"Cosy-Clientip":          {"127.0.0.1"},
		"Cosy-Bodyhash":          {bodyHash},
		"Cosy-Bodylength":        {fmt.Sprintf("%d", len(body))},
		"Cosy-Sigpath":           {sigPath},
		"Cosy-Data-Policy":       {cosyDataPolicy},
		"Cosy-Organization-Id":   {""},
		"Cosy-Organization-Tags": {""},
		"Login-Version":          {cosyLoginVersion},
		"X-Request-Id":           {xRequestID},
	}, nil
}
