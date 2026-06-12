package crypto

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/asker/asker/platform/tenancy"
)

func TestMemDEKStoreGetMissing(t *testing.T) {
	store := NewMemDEKStore()
	if _, err := store.GetWrappedDEK(context.Background(), "tenant-a"); !errors.Is(err, ErrDEKNotFound) {
		t.Errorf("Get on empty store: err = %v, want ErrDEKNotFound", err)
	}
}

func TestMemDEKStorePutGetRoundTrip(t *testing.T) {
	store := NewMemDEKStore()
	ctx := context.Background()
	want := []byte("wrapped-dek-a")

	if err := store.PutWrappedDEK(ctx, "tenant-a", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.GetWrappedDEK(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Get = %q, want %q", got, want)
	}

	// Tenants are isolated.
	if _, err := store.GetWrappedDEK(ctx, "tenant-b"); !errors.Is(err, ErrDEKNotFound) {
		t.Errorf("Get other tenant: err = %v, want ErrDEKNotFound", err)
	}
}

func TestMemDEKStoreFirstWriterWins(t *testing.T) {
	store := NewMemDEKStore()
	ctx := context.Background()
	first := []byte("first")
	second := []byte("second")

	if err := store.PutWrappedDEK(ctx, "tenant-a", first); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := store.PutWrappedDEK(ctx, "tenant-a", second); err != nil {
		t.Fatalf("second Put: %v (want idempotent nil)", err)
	}
	got, err := store.GetWrappedDEK(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, first) {
		t.Errorf("Get = %q, want first writer %q", got, first)
	}
}

func TestMemDEKStoreCopiesValues(t *testing.T) {
	store := NewMemDEKStore()
	ctx := context.Background()

	in := []byte("wrapped")
	if err := store.PutWrappedDEK(ctx, "tenant-a", in); err != nil {
		t.Fatalf("Put: %v", err)
	}
	in[0] = 'X' // caller mutates its slice after Put

	out1, err := store.GetWrappedDEK(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(out1, []byte("wrapped")) {
		t.Errorf("stored value affected by caller mutation: %q", out1)
	}
	out1[0] = 'Y' // caller mutates the returned slice

	out2, err := store.GetWrappedDEK(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(out2, []byte("wrapped")) {
		t.Errorf("stored value affected by returned-slice mutation: %q", out2)
	}
}

func TestMemDEKStoreInputValidation(t *testing.T) {
	store := NewMemDEKStore()
	ctx := context.Background()

	if err := store.PutWrappedDEK(ctx, "", []byte("w")); !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("Put empty tenant: err = %v, want ErrNoTenant", err)
	}
	if _, err := store.GetWrappedDEK(ctx, ""); !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("Get empty tenant: err = %v, want ErrNoTenant", err)
	}
	if err := store.PutWrappedDEK(ctx, "tenant-a", nil); err == nil {
		t.Error("Put empty wrapped DEK succeeded, want error")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.PutWrappedDEK(canceled, "tenant-a", []byte("w")); !errors.Is(err, context.Canceled) {
		t.Errorf("Put canceled ctx: err = %v, want context.Canceled", err)
	}
	if _, err := store.GetWrappedDEK(canceled, "tenant-a"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get canceled ctx: err = %v, want context.Canceled", err)
	}
}
