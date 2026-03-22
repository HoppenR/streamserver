package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func createSignedState(secret []byte) (string, error) {
	now := time.Now().Unix()
	payload := fmt.Sprintf("%d", now)

	hash := hmac.New(sha256.New, secret)
	payloadBytes := []byte(payload)
	n, err := hash.Write(payloadBytes)
	if err != nil || n != len(payloadBytes) {
		return "", fmt.Errorf("failed to write to hmac: %v", err)
	}
	signature := base64.RawURLEncoding.EncodeToString(hash.Sum(nil))

	return fmt.Sprintf("%s:%s", payload, signature), nil
}

func verifySignedState(state string, secret []byte) bool {
	parts := strings.Split(state, ":")
	if len(parts) != 2 {
		return false
	}

	payload, providedSig := parts[0], parts[1]

	hash := hmac.New(sha256.New, secret)
	payloadBytes := []byte(payload)
	n, err := hash.Write(payloadBytes)
	if err != nil || n != len(payloadBytes) {
		return false
	}
	expectedSig := base64.RawURLEncoding.EncodeToString(hash.Sum(nil))

	if !hmac.Equal([]byte(providedSig), []byte(expectedSig)) {
		return false
	}

	ts, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return false
	}

	if time.Now().Unix()-ts > 60 {
		return false
	}

	return true
}
