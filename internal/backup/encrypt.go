package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
)

// EncryptionAge is the value of Meta.Encryption for age-encrypted dumps.
const EncryptionAge = "age"

// ParseRecipients parses age public keys (age1...). It rejects an empty list,
// so encryption cannot be enabled without a key by mistake.
func ParseRecipients(keys []string) ([]age.Recipient, error) {
	var lines []string
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			lines = append(lines, k)
		}
	}
	if len(lines) == 0 {
		return nil, errors.New("backup: encryption is enabled but encryption.recipients is empty")
	}
	rs, err := age.ParseRecipients(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		return nil, fmt.Errorf("backup: encryption.recipients: %w", err)
	}
	return rs, nil
}

// LoadIdentities reads an age identity file (as written by age-keygen), used
// by verify and restore to decrypt a dump.
func LoadIdentities(path string) ([]age.Identity, error) {
	if path == "" {
		return nil, errors.New("backup: encryption.identityFile is empty")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("backup: open identity file: %w", err)
	}
	defer f.Close()
	ids, err := age.ParseIdentities(f)
	if err != nil {
		return nil, fmt.Errorf("backup: parse identity file %s: %w", path, err)
	}
	return ids, nil
}

// Decrypt returns a reader of the plaintext of an age-encrypted stream.
func Decrypt(r io.Reader, ids ...age.Identity) (io.Reader, error) {
	pr, err := age.Decrypt(r, ids...)
	if err != nil {
		return nil, fmt.Errorf("backup: decrypt: %w", err)
	}
	return pr, nil
}
