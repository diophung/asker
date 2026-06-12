# platform/proto

Canonical protobuf contracts for Asker, plus the generated Go code that the rest
of the repo imports.

## Layout

```
platform/proto/
├── buf.yaml                      # buf v2 module config (lint: STANDARD, breaking: FILE)
├── buf.gen.yaml                  # codegen config (protoc-gen-go -> gen/go)
├── asker/v1/document.proto       # canonical Document model (spec §2.4)
└── gen/go/asker/v1/              # generated Go (committed; do not edit by hand)
```

The `Document` message is the contract every component shares: connectors emit
it, Kafka topics carry it, index writers and blob GC consume it. Deletions
travel as documents too, via the `Tombstone` field.

Import the generated code as:

```go
import askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
```

## Regenerating

From the repo root:

```sh
make tools   # installs buf and protoc-gen-go into ./bin
make proto   # runs `buf generate` in platform/proto with ./bin on PATH
```

Run `buf lint` before committing proto changes, and commit the regenerated
`gen/go` output alongside the `.proto` change. CI enforces both: the lint job
runs `buf lint` and regenerates the code, failing if `gen/go` has drifted from
the `.proto` sources.

## Lint configuration

`buf.yaml` uses the STANDARD lint ruleset with one exception:
`ENUM_VALUE_PREFIX` is disabled because the M0 contract fixes the `DocType`
values as `EMAIL`, `CHAT_MESSAGE`, ... without a `DOC_TYPE_` prefix (the zero
value keeps the standard `DOC_TYPE_UNSPECIFIED` form).
