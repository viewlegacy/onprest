# Contributing

Thanks for helping improve Onprest.

## Development

```sh
go test ./...
go vet ./...
gofmt -w cmd internal
```

Keep changes focused. The gateway must remain stateless, and SQL/database credentials must stay agent-side only.

## Pull Requests

Include:

- What changed
- Security impact
- Test coverage or manual verification

Do not add dashboard or managed-service-only code to the OSS core.
