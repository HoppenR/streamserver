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

func createSignedState(secret []byte) string {
	now := time.Now().Unix()
	payload := fmt.Sprintf("%d", now)

	h := hmac.New(sha256.New, secret)
	h.Write([]byte(payload))
	signature := base64.RawURLEncoding.EncodeToString(h.Sum(nil))

	return fmt.Sprintf("%s:%s", payload, signature)
}

func verifySignedState(state string, secret []byte) bool {
	parts := strings.Split(state, ":")
	if len(parts) != 2 {
		return false
	}

	payload, providedSig := parts[0], parts[1]

	h := hmac.New(sha256.New, secret)
	h.Write([]byte(payload))
	expectedSig := base64.RawURLEncoding.EncodeToString(h.Sum(nil))

	if !hmac.Equal([]byte(providedSig), []byte(expectedSig)) {
		return false
	}

	ts, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return false
	}

	if time.Now().Unix()-ts > 600 {
		return false
	}

	return true
}
