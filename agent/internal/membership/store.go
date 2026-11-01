package membership

import (
	"log/slog"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// The pinned operator keys and the revocation list are the two pieces of
// state that outlive any one manifest.

func saveTrust(path string, ops []identity.Key) error {
	b, err := yaml.Marshal(map[string][]identity.Key{"operators": ops})
	if err != nil {
		return err
	}
	return writeFile(path, b, 0o644)
}

func loadTrust(path string) ([]identity.Key, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string][]identity.Key
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return doc["operators"], nil
}

type revocationFile struct {
	Revocations []*identity.SignedRevocation `cbor:"revocations"`
}

func saveRevocations(path string, revs []*identity.SignedRevocation) error {
	b, err := identity.Marshal(revocationFile{Revocations: revs})
	if err != nil {
		return err
	}
	return writeFile(path, b, 0o644)
}

func loadRevocations(path string, lg *slog.Logger) []*identity.SignedRevocation {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc revocationFile
	if err := identity.Unmarshal(b, &doc); err != nil {
		lg.Warn("revocation file unreadable, starting without it", "err", err)
		return nil
	}
	var out []*identity.SignedRevocation
	for _, r := range doc.Revocations {
		if r.Verify() == nil {
			out = append(out, r)
		}
	}
	return out
}
