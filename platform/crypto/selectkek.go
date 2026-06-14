package crypto

import (
	"cmp"
	"errors"
	"fmt"
)

// KEKSelection configures SelectKEK, the single place that decides between the
// production Vault Transit KEK and the dev file KEK (ADR-015 §3). Both the
// control plane and the connector hub call it so the choice — and the prod
// fail-closed guard — are identical and tested once.
type KEKSelection struct {
	// VaultAddr, when non-empty, selects NewVaultKEK. Empty selects NewFileKEK.
	VaultAddr string
	// VaultToken / VaultKeyName configure the Vault path. VaultKeyName defaults
	// to "asker-kek" when empty; control-plane and connector-hub MUST agree on
	// it so a DEK wrapped by one unwraps in the other.
	VaultToken   string
	VaultKeyName string
	// KEKFile is the dev FileKEK path, used only when VaultAddr is empty.
	KEKFile string
	// IsProd marks a production deployment. When true and VaultAddr is empty,
	// SelectKEK FAILS CLOSED rather than mint an ephemeral dev file KEK — a prod
	// deployment must use Vault (ADR-015). On a non-prod deployment an empty
	// VaultAddr is fine (the dev inner loop keeps the zero-dependency shim).
	IsProd bool
}

// ErrProdRequiresVault is returned by SelectKEK when a production deployment
// (IsProd) is configured without a VAULT_ADDR. It is fatal at startup.
var ErrProdRequiresVault = errors.New("crypto: production deployment requires VAULT_ADDR (refusing the dev file KEK)")

// SelectKEK returns the KEKProvider for the current environment: Vault Transit
// when VaultAddr is set, otherwise the dev file KEK — except in production with
// no Vault, where it fails closed (ErrProdRequiresVault). The returned provider
// implements the exact same interface in both cases, so the rest of the
// envelope-crypto lifecycle is unchanged.
func SelectKEK(sel KEKSelection) (KEKProvider, error) {
	if sel.VaultAddr != "" {
		return NewVaultKEK(VaultConfig{
			Addr:    sel.VaultAddr,
			Token:   sel.VaultToken,
			KeyName: cmp.Or(sel.VaultKeyName, "asker-kek"),
		})
	}
	if sel.IsProd {
		return nil, ErrProdRequiresVault
	}
	kek, err := NewFileKEK(sel.KEKFile)
	if err != nil {
		return nil, fmt.Errorf("crypto: load file KEK %q: %w", sel.KEKFile, err)
	}
	return kek, nil
}
