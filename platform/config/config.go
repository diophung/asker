// Package config loads service configuration from environment variables.
package config

import (
	"fmt"
	"reflect"

	"github.com/caarlos0/env/v11"
)

// Load populates dst from environment variables using caarlos0/env struct
// tags (env:"NAME" envDefault:"..."). prefix is prepended to every tag name,
// e.g. Load("GATEWAY_", &cfg) with a field tagged env:"ADDR" reads GATEWAY_ADDR.
// dst must be a non-nil pointer to a struct.
func Load(prefix string, dst any) error {
	v := reflect.ValueOf(dst)
	if dst == nil || v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("config: dst must be a non-nil struct pointer, got %T", dst)
	}
	if err := env.ParseWithOptions(dst, env.Options{Prefix: prefix}); err != nil {
		return fmt.Errorf("config: parse environment: %w", err)
	}
	return nil
}
