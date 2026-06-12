package sdk

import (
	"crypto/sha256"
	"encoding/hex"
)

// DocID derives the stable canonical document ID for a source record: the
// lowercase hex SHA-256 of connectorID + ":" + sourceNativeID. This is THE
// doc_id rule (spec §2.4) — every component that writes or matches doc_ids
// (connectors, hub, index writers, tombstone handling) must use this
// function so that re-syncs and deletes converge on the same ID.
//
// Uniqueness across connectors relies on connector IDs never containing the
// ":" separator; Spec.ID is restricted to [a-z0-9-]+ (enforced by
// connectortest.RunSpecChecks), which guarantees it.
func DocID(connectorID, sourceNativeID string) string {
	sum := sha256.Sum256([]byte(connectorID + ":" + sourceNativeID))
	return hex.EncodeToString(sum[:])
}
