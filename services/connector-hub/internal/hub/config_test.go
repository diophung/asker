package hub

import (
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Addr != ":9300" || cfg.HealthAddr != ":9301" {
		t.Errorf("addrs = %q / %q", cfg.Addr, cfg.HealthAddr)
	}
	if cfg.WebhookBase != "http://connector-hub:9300" {
		t.Errorf("WebhookBase = %q", cfg.WebhookBase)
	}
	if cfg.ControlPlaneAddr != "dns:///control-plane:9100" {
		t.Errorf("ControlPlaneAddr = %q", cfg.ControlPlaneAddr)
	}
	if cfg.SyncInterval != 30*time.Second || cfg.SchedulerTick != 10*time.Second {
		t.Errorf("intervals = %v / %v", cfg.SyncInterval, cfg.SchedulerTick)
	}
	if cfg.KEKFile != "/keys/kek.bin" {
		t.Errorf("KEKFile = %q", cfg.KEKFile)
	}
	if len(cfg.Kafka.Brokers) != 1 || cfg.Kafka.Brokers[0] != "redpanda:9092" {
		t.Errorf("Kafka.Brokers = %v", cfg.Kafka.Brokers)
	}
	if cfg.MinIOEndpoint != "minio:9000" || cfg.MinIOBucket != "asker-blobs" || cfg.MinIOUseSSL {
		t.Errorf("minio = %q bucket %q ssl %v", cfg.MinIOEndpoint, cfg.MinIOBucket, cfg.MinIOUseSSL)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	t.Setenv("HUB_ADDR", ":7300")
	t.Setenv("CONNECTOR_SYNC_INTERVAL", "5s")
	t.Setenv("SCHEDULER_TICK", "1s")
	t.Setenv("KAFKA_BROKERS", "a:1,b:2")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Addr != ":7300" || cfg.SyncInterval != 5*time.Second || cfg.SchedulerTick != time.Second {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if len(cfg.Kafka.Brokers) != 2 {
		t.Errorf("Kafka.Brokers = %v", cfg.Kafka.Brokers)
	}
}

func TestLoadConfigRejectsNonPositiveIntervals(t *testing.T) {
	t.Setenv("CONNECTOR_SYNC_INTERVAL", "0s")
	if _, err := LoadConfig(); err == nil {
		t.Error("zero sync interval accepted")
	}
	t.Setenv("CONNECTOR_SYNC_INTERVAL", "30s")
	t.Setenv("SCHEDULER_TICK", "-1s")
	if _, err := LoadConfig(); err == nil {
		t.Error("negative scheduler tick accepted")
	}
}
