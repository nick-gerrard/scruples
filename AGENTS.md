# Scruples — Agent & Contributor Guide

## What This App Is

Scruples is a multiplayer party game of crowdsourced moral dilemmas. Players write yes/no dilemma cards into a shared deck, then join a single communal game in either **classic** mode (guess what the drawer would actually do, race to `point_goal`, optionally call bluff) or **freeplay** mode (endless dilemmas where everyone votes for themselves).

Stack: Go 1.26, SQLite via the pure-Go `modernc.org/sqlite` driver, server-rendered `html/template`, htmx 2.0.4 polling, and Tailwind CSS via CDN. Deployed automatically by `roost` on push to `main` (see `deploy.toml`).

## Package Map

- `main.go`: reads env vars, opens SQLite, seeds the admin account (`nick`), wires routes in `buildMux`, and serves HTTP.
- `internal/db`: `Open(path)` configures SQLite (`foreign_keys(1)`, WAL mode, `SetMaxOpenConns(1)`) and runs versioned migrations from `migrate.go`.
- `internal/web`: `App` dependency container (`*sql.DB`), cookie sessions (`scruples_session`, 30-day TTL), `CurrentUser`, `RequireUser` (401 for htmx, 303 redirect to `/login` otherwise), and template helpers (`Render`, `RenderFragment`).
- `internal/auth`: `SeedAdmin`, bcrypt auth, and routes for `/login`, `/signup`, `/logout`, `/account`, and `/account/password`.
- `internal/cards`: deck routes (`/cards`, `/cards/new`, `POST /cards`, `DELETE /cards/{id}` — archives the card; author or admin only).
- `internal/api`: JSON card API (`/api/cards...`) enabled only when `SCRUPLES_API_KEY` is set, with Bearer auth, `?dry_run=1` support, and `api_audit` logging.
- `internal/game`:
  - `game.go`: HTTP handlers for `/game`, `/game/state`, `/game/{start,join,draw,answer,vote,challenge,challenge/vote,next,end}`, and `/winners`.
  - `logic.go`: game state transitions (`doStart`, `doJoin`, `pickCard`, `doDraw`, `maybeReveal`, `awardAndFinish`, `doNext`, `maybeResolveChallenge`, `doEnd`).
  - `view.go`: `querier` interface (`*sql.DB` / `*sql.Tx`), query helpers, turn rotation (`nextDrawerIDFor`), and view models (`buildView`, `buildHall`).
- `templates/`: server-rendered HTML templates (`layout.html`, `home.html`, `login.html`, `signup.html`, `account.html`, `cards.html`, `card_new.html`, `game.html`, `winners.html`), parsed per request from `templates/`.

## Game Flow

- One active game runs at a time (`games.status`: `active` -> `finished`).
- Round lifecycle (`rounds.status`): `voting` -> `revealed` -> `challenge` -> `complete` (or directly `revealed` -> `complete` when accepted via `POST /game/next`).
- **Eligibility**: players whose `joined_at <= round.created_at` participate in the active round; mid-round joiners start on the next card.
- **Classic mode**: the current drawer answers secretly (`POST /game/answer`) while eligible non-drawers guess (`POST /game/vote`). Once all are in, `maybeReveal` flips status to `revealed`. Any joined player can accept (`POST /game/next`), locking `final_answer = drawer_answer`, awarding +1 to matching initial voters, and finishing the game at `point_goal`. Any joined non-drawer can instead call bluff (`POST /game/challenge` -> `challenge`); eligible non-drawers re-vote (`POST /game/challenge/vote`), and a strict majority contradicting the drawer flips `final_answer` before scoring initial votes.
- **Freeplay mode**: every eligible player (including the drawer) votes for themselves; `maybeReveal` shows tallies, and `POST /game/next` marks the round `complete`.
- **Live updates**: `#game-state` in `templates/game.html` polls `GET /game/state` every 2s (`hx-trigger="every 2s"`). Every valid htmx game action returns the `game-state` fragment (a bad value on `/game/answer`, `/game/vote` or `/game/challenge/vote` gets a plain 400) to swap into `#game-state` (non-htmx requests redirect 303 to `/game`).

## Card API

- Enabled only when `SCRUPLES_API_KEY` is non-empty; otherwise every `/api/` path returns 404.
- **Auth**: `Authorization: Bearer <key>` compared via SHA-256 + `crypto/subtle.ConstantTimeCompare` (401 JSON `{"error":"unauthorized"}` on missing/wrong key). JSON only; no cookies or sessions.
- **Endpoints**: `GET /api/cards` (`?source=human|ai&archived=0|1|all&author=&q=&limit=&offset=`, default `archived=0`, `limit=500` max `1000`), `GET /api/cards/{id}`, `POST /api/cards` (defaults `source="ai"`, `author="Claude"`; bot authors only), `PATCH /api/cards/{id}`, `POST /api/cards/{id}/archive`, `POST /api/cards/{id}/restore`, and `POST /api/cards/batch` (max 200 ops, all-or-nothing single transaction).
- **Dry runs & audit**: every write accepts `?dry_run=1` (runs in a transaction, rolls back, returns what would have happened with `"dry_run": true`). Every successful write is audited with before/after JSON in `api_audit` (for dry runs, recorded after rollback).
- **Bot user**: migration 3 seeds the `Claude` user (`is_bot = 1`, `password_hash = '!'`) as the default AI card author; `POST /login` always refuses `is_bot` users.

## Running Locally & Testing

Environment variables (all optional locally):

- `SCRUPLES_ADDR`: listen address (default `:8080`).
- `SCRUPLES_DB`: SQLite file path (default `scruples.db`).
- `SCRUPLES_ADMIN_PASSWORD`: initial password for the seeded `nick` admin user (if unset when `nick` does not exist yet, a random 16-byte hex password is generated and logged once; set `SCRUPLES_RESET_ADMIN_PASSWORD=1` to force-reset on startup).
- `SCRUPLES_API_KEY`: Bearer token enabling `/api/*` (omit to disable the API; in production this lives in the server's roost env file, never in `deploy.toml` or git).

Common commands (see `Makefile`):

```sh
make dev     # go run . with SCRUPLES_ADDR=127.0.0.1:8080 SCRUPLES_DB=dev.db SCRUPLES_ADMIN_PASSWORD=devpass
make test    # go test ./...
make vet     # go vet ./...
make build   # go build -o scruples .
```

## Conventions

- **MOBILE-FIRST**: design layouts, typography, and touch targets for ~390px wide phones first (the layout caps content at `max-w-2xl` with large full-width tap targets).
- **Never call `app.DB` inside an open transaction**: `db.Open` calls `SetMaxOpenConns(1)`, so querying `app.DB` while holding a `*sql.Tx` will deadlock. Pass the `*sql.Tx` (via `querier`) to any helper called inside a transaction.
- **Keep handlers' state checks inside transactions**: check current game/round state and apply mutations in the same transaction to avoid check-then-act races.
- **No secrets in git**: `*.db` files are gitignored; production `SCRUPLES_ADMIN_PASSWORD` and `SCRUPLES_API_KEY` live in the server's roost env file, never in `deploy.toml` or source code.
- **Schema changes**: append a new entry to `migrations` in `internal/db/migrate.go` with the next integer `Version`; never edit a migration that has already been applied.
- **Branch & PR workflow**: always work on a branch and open a pull request. `main` is protected, and merging to `main` automatically deploys to production via `roost`.
