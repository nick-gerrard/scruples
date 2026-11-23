---
name: scruples-api
description: Use the Scruples JSON card API to list, create, patch, archive, restore, and batch-edit dilemma cards with Bearer auth and dry-run previews.
---

# Scruples Card API

Base URL: `https://scruples.nickgerrard.dev`
Auth: `Authorization: Bearer $SCRUPLES_API_KEY` (all responses are JSON; no cookies/sessions).

Always preview writes with `?dry_run=1` first — dry runs execute inside a transaction, roll back without modifying `cards`, return the exact result that would occur, and still record an `api_audit` entry.

## List & Inspect Cards

```sh
# Active cards (default archived=0); supports source=human|ai, archived=0|1|all, author=<name>, q=<substring>
curl -sS -H "Authorization: Bearer $SCRUPLES_API_KEY" \
  "https://scruples.nickgerrard.dev/api/cards?source=ai&archived=0"

# Fetch a single card by ID
curl -sS -H "Authorization: Bearer $SCRUPLES_API_KEY" \
  "https://scruples.nickgerrard.dev/api/cards/42"
```

## Dry Run Before Writing

```sh
curl -sS -X POST \
  -H "Authorization: Bearer $SCRUPLES_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"circumstance":"You find $50 in a library book.","question":"Do you hand it in to the librarian?"}' \
  "https://scruples.nickgerrard.dev/api/cards?dry_run=1"
```

## Create, Patch, Archive, Restore

```sh
# Create (defaults: source="ai", author="Claude"; limits: 2000/300 chars)
curl -sS -X POST \
  -H "Authorization: Bearer $SCRUPLES_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"circumstance":"You find $50 in a library book.","question":"Do you hand it in to the librarian?"}' \
  "https://scruples.nickgerrard.dev/api/cards"

# Patch any of circumstance, question, source
curl -sS -X PATCH \
  -H "Authorization: Bearer $SCRUPLES_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"question":"Do you turn it in to the front desk?"}' \
  "https://scruples.nickgerrard.dev/api/cards/42"

# Archive / Restore
curl -sS -X POST -H "Authorization: Bearer $SCRUPLES_API_KEY" \
  "https://scruples.nickgerrard.dev/api/cards/42/archive"

curl -sS -X POST -H "Authorization: Bearer $SCRUPLES_API_KEY" \
  "https://scruples.nickgerrard.dev/api/cards/42/restore"
```

## Atomic Batch Operations

Runs all ops (`create`, `patch`, `archive`, `restore`) in a single all-or-nothing transaction. Preview with `?dry_run=1` first.

```sh
curl -sS -X POST \
  -H "Authorization: Bearer $SCRUPLES_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "ops": [
      {"op":"create","circumstance":"Scene...","question":"Would you?"},
      {"op":"patch","id":12,"question":"Refined question?"},
      {"op":"archive","id":15}
    ]
  }' \
  "https://scruples.nickgerrard.dev/api/cards/batch?dry_run=1"
```
