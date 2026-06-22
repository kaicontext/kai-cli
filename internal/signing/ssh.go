package signing

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
	"kai/internal/util"
)

// SignChangeSetPayloadSSH signs a ChangeSet payload (excluding signature fields).
func SignChangeSetPayloadSSH(payload map[string]interface{}, keyPath string) (map[string]interface{}, error) {
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("reading key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing key: %w", err)
	}

	unsigned := cloneWithoutSignature(payload)
	data, err := util.CanonicalJSON(unsigned)
	if err != nil {
		return nil, fmt.Errorf("canonical json: %w", err)
	}

	sig, err := signer.Sign(rand.Reader, data)
	if err != nil {
		return nil, fmt.Errorf("signing: %w", err)
	}

	fingerprint := ssh.FingerprintSHA256(signer.PublicKey())
	return map[string]interface{}{
		"signature": base64.StdEncoding.EncodeToString(sig.Blob),
		"sigFormat": sig.Format,
		"sigType":   "ssh",
		"signer":    fingerprint,
		"signedAt":  util.NowMs(),
	}, nil
}

func cloneWithoutSignature(payload map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(payload))
	for k, v := range payload {
		switch k {
		case "signature", "sigFormat", "sigType", "signer", "signedAt":
			continue
		default:
			out[k] = v
		}
	}
	return out
}
