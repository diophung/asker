package personalization

// Redis key contract for the personalization read path (DECISIONS D4). The
// gateway write-through-caches a tenant's resolved Profile and LearnedModel
// under these keys whenever they change; the query service reads them on the hot
// path (falling back to DefaultProfile on a miss). Defining the format here, as
// pure string functions, keeps the producer (gateway) and consumer (query) from
// drifting. The tenant alphabet (platform/tenancy) excludes Redis glob
// metacharacters, so the GDPR purge can safely match "asker:pref:<tenant>".

// RedisProfileKey is the key holding a tenant's resolved Profile JSON.
func RedisProfileKey(tenantID string) string { return "asker:pref:" + tenantID }

// RedisWeightsKey is the key holding a tenant's LearnedModel JSON.
func RedisWeightsKey(tenantID string) string { return "asker:weights:" + tenantID }
