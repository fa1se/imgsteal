# imgsteal

A tiny reverse proxy that forwards a request to a target URL while overriding
the `Referer` header based on regex rules matched against the target's domain.
The target is passed url-encoded in the `url` query parameter; all other
request headers are passed through unchanged. If no rule matches, the `Referer`
is left untouched.

```
GET /?url=https%3A%2F%2Fexample.com%2Fimage.jpg
```

## Run

```sh
go run . -listen :8080 -rules rules.json -v
```

### Flags

| Flag       | Default      | Description                                  |
| ---------- | ------------ | -------------------------------------------- |
| `-listen`  | `:8080`      | address to listen on                         |
| `-rules`   | `rules.json` | path to the JSON rules file (optional)       |
| `-timeout` | `30s`        | upstream request timeout                     |
| `-v`       | (not set)    | log loaded rules on startup and each request |

An explicit flag always wins over the corresponding environment variable.

## Rules

A rule maps a domain regex to the `Referer` to send. The first matching rule
wins. Rules can come from the JSON file, from the environment, or both (file
first, then environment).

`rules.json`:

```json
{
  "rules": [
    { "match": "(^|\\.)example\\.com$", "referer": "https://example.com/" }
  ]
}
```

## Environment variables

| Variable                      | Equivalent of      |
| ----------------------------- | ------------------ |
| `IMGSTEAL_LISTEN`             | `-listen`          |
| `IMGSTEAL_TIMEOUT`            | `-timeout`         |
| `IMGSTEAL_VERBOSE`            | `-v`               |
| `IMGSTEAL_RULE_MATCH[_XXX]`   | a rule's `match`   |
| `IMGSTEAL_RULE_REFERER[_XXX]` | a rule's `referer` |

A rule is a `MATCH`/`REFERER` pair sharing the same optional `_XXX` suffix:

```sh
IMGSTEAL_RULE_MATCH='(^|\.)example\.com$' \
IMGSTEAL_RULE_REFERER='https://example.com/' \
imgsteal -v
```

## Container image

Pull from GitHub Container Registry:

```sh
podman run --rm -p 8080:8080 \
  -e IMGSTEAL_RULE_MATCH='(^|\.)example\.com$' \
  -e IMGSTEAL_RULE_REFERER='https://example.com/' \
  ghcr.io/fa1se/imgsteal:latest
```

Or mount a rules file instead of using env vars:

```sh
podman run --rm -p 8080:8080 -v ./rules.json:/rules.json:ro \
  ghcr.io/fa1se/imgsteal:latest
```
