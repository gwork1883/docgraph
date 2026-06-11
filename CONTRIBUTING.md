# Contributing

Thanks for helping improve DocGraph.

## Development

```bash
make test
make build
```

Before opening a pull request:

- Run `make test`.
- Keep generated binaries, local databases, caches, tokens, cookies, and private keys out of commits.
- Use example domains such as `example.com`, `docs.example`, or `confluence.example` in tests and docs.
- Avoid committing machine-local paths unless they are clearly test fixtures.

## Commit Messages

Use concise commit messages that describe the user-facing or developer-facing change.

## Security

Do not include real credentials or internal hostnames in issues, pull requests, tests, fixtures, or screenshots. Follow [SECURITY.md](SECURITY.md) for vulnerability reports.
