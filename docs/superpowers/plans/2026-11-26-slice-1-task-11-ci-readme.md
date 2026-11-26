# Slice 1, Task 11 — CI Pipeline + README Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every push and PR to `main` runs lint, unit tests, integration tests, a generated-code drift check, a vulnerability scan and the full compose smoke test on GitHub Actions; the repo gets a README a stranger can start from. This closes slice 1 ("CI green on `main`").

**Architecture:** One workflow file, `.github/workflows/ci.yml`, with independent parallel jobs. Jobs call the existing Makefile targets wherever one exists, so CI and a laptop run the same commands (one source of truth). Only `golangci-lint` runs through its official action (for caching), pinned to the same version as the Makefile.

**Tech Stack:** GitHub Actions (`ubuntu-24.04` runners, Docker preinstalled), `actions/setup-go`, `golangci/golangci-lint-action`, `buf` via `tools/go.mod`, `govulncheck`, `actionlint` (local check only).

**Spec:** `docs/SPEC.md` §13 (tooling, boundaries, CI); refines Task 11 of `docs/superpowers/plans/2026-11-23-slice-1-walking-skeleton.md`. Where the two differ, this plan wins (differences are listed under "Corrections to the slice plan").

## Global Constraints

- Every third-party action is pinned to a **full commit SHA** with the version in a trailing comment (`uses: actions/checkout@<sha> # v5.x.y`). Look up the current release SHA at implementation time.
- Every tool version is pinned; nothing runs `@latest`.
- `golangci-lint` version in CI == `GOLANGCI_LINT_VERSION` in the Makefile (`v2.15.0`). A comment in both places points at the other.
- Workflow `permissions: contents: read` at top level; no job gets more.
- `concurrency: { group: ci-${{ github.ref }}, cancel-in-progress: true }` so a new push cancels the stale run.
- Every job has `timeout-minutes` (lint/test/vuln/generated 10, test-integration 15, e2e 20).
- `actions/setup-go`: `go-version-file: go.mod`, `cache-dependency-path: | go.sum \n tools/go.sum`.
- Commit messages carry **no** `Co-Authored-By` trailer (author's standing instruction).
- Branch: `slice-1-task-11-ci` from `main` (`6a922a6`).

## Corrections to the slice plan

1. Plan says `go tool buf …`; buf lives in `tools/go.mod`, so it is `go tool -modfile=tools/go.mod buf …` (the Makefile's `$(TOOL)`).
2. Plan says `git diff --exit-code` for drift; that misses **new untracked** generated files. Use `git status --porcelain` and fail if non-empty.
3. Plan says `buf breaking --against '.git#branch=main'`; `actions/checkout` is shallow and has no local `main`, so that fails. Run it **only on `pull_request`**, with `fetch-depth: 0`, against `.git#ref=${{ github.event.pull_request.base.sha }}`.
4. Plan says `govulncheck@latest`; pin it (see Global Constraints).
5. `docs/img/slice1-trace.png` (Task 10 Step 5) does **not exist** yet. The README links it, so the author captures it in Task 11b Step 1, or the README ships without it and the link is added later.

## Review Focus

1. **Generated code drift via a brand-new file** (someone adds a `.proto`/query but doesn't commit the output): `generated` job fails. → Task 11a Step 4 (deliberate-drift check).
2. **Smoke test fails in CI:** the job log must contain `docker compose ps` and the last 200 lines of every container's logs, or nobody can debug it. → Task 11a, `e2e` job `if: failure()` step.
3. **Breaking proto change in a PR** (renumbered field): `lint` job fails on the PR. → Task 11a Step 5.
4. **Lint version skew** between laptop and CI: same version pinned in both, cross-referenced. → Global Constraints.
5. **Slow first run:** `otel-lgtm` + Kafka images are large; `make up` already has `--wait-timeout 180`, and the e2e job's 20-minute timeout bounds a hung stack.

---

### Task 11a: CI workflow

**Files:**
- Create: `.github/workflows/ci.yml`
- Modify: `Makefile` (comment on `GOLANGCI_LINT_VERSION` pointing at `ci.yml`; new `gen-check` target)

**Interfaces:**
- Consumes (Makefile): `test`, `test-int`, `gen`, `up`, `smoke`, `ps`, `$(TOOL)`.
- Produces: `make gen-check` — runs `make gen`, then fails with the list of changed files if `git status --porcelain` is non-empty. Usable locally before pushing.

Triggers: `push` to `main`, `pull_request` targeting `main`.

| Job | Steps after checkout + setup-go |
|---|---|
| `lint` | `golangci-lint-action` with `version: v2.15.0`; `go tool -modfile=tools/go.mod buf lint`; on `pull_request` only: `buf breaking --against ".git#ref=${{ github.event.pull_request.base.sha }}"` (checkout `fetch-depth: 0` in this job) |
| `test` | `make test` |
| `test-integration` | `make test-int` (testcontainers uses the runner's Docker) |
| `generated` | `make gen-check` (sqlc runs from its Docker image — fine on the runner) |
| `vuln` | `go run golang.org/x/vuln/cmd/govulncheck@<pinned vX.Y.Z> ./...` |
| `e2e` | `make up` → `make smoke`; `if: failure()`: `make ps` and `docker compose -f deploy/compose/docker-compose.yml logs --no-color --tail=200` |

- [ ] **Step 1: Add `gen-check` to the Makefile** (and to `.PHONY`, with a `##` help line like the other targets).
- [ ] **Step 2: Verify locally.** `make gen-check` → exit 0 on a clean tree.
- [ ] **Step 3: Write `ci.yml`** per the table and Global Constraints. Lint it: `go run github.com/rhysd/actionlint/cmd/actionlint@<pinned> ` → no output.
- [ ] **Step 4: Prove the drift check bites.** Edit a message's leading comment in `proto/deliverit/order/v1/order.proto` (it flows into the generated Go doc comment) and commit **only the proto** on a throwaway local commit, then `make gen-check` → non-zero, listing only the changed `gen/` file. Revert with `git checkout -- gen && git reset --hard HEAD~1`. (Don't test it by adding a stray file under `gen/`: buf's `clean: true` deletes it, so that test would pass for the wrong reason.)
- [ ] **Step 5: Push the branch, open a PR** (`gh pr create --fill`), `gh pr checks --watch` → all six jobs green. Then, on a throwaway commit, renumber a field in `order.proto` → `lint` fails on `buf breaking`; drop the commit (`git reset --hard HEAD~1 && git push --force-with-lease`).
- [ ] **Step 6: Stop for author review.** Commit after approval: `ci: lint, tests, drift, vuln and e2e workflow`.

---

### Task 11b: README

**Files:**
- Create: `README.md`, optionally `docs/img/slice1-trace.png`

**Interfaces:**
- Consumes: Makefile targets and help lines; compose ports (gateway 8080, Grafana 3000, Kafka UI 8090).

Sections, in order: one paragraph on what DeliverIt is and why (from `PLANNING.md` intro) · CI badge (`ci.yml` on `main`) · status: "slice 1 of 8 — walking skeleton" · prerequisites (Docker with compose v2, Go per `go.mod`, `curl`, `jq`) · quick start: `make up`, `make smoke`, `make down` / `make clean` · development: `make test`, `make test-int`, `make lint`, `make gen`, `make run-order` / `make run-gateway` with `make db-up` · local URLs table · repo layout (top level only, one line each) · links to `docs/SPEC.md` and `docs/PLANNING.md` · the trace screenshot.

`docs/PLANNING.md` is git-ignored ("personal"), so the README links **only `docs/SPEC.md`** — a link to an untracked file would 404 on GitHub.

- [ ] **Step 1 (author): capture the trace screenshot.** `make up && make smoke`, open the printed Grafana link, save the trace view to `docs/img/slice1-trace.png`. Skip → README omits the image.
- [ ] **Step 2: Write `README.md`.** Every command in it is copy-pasted from the Makefile; every URL from the compose file.
- [ ] **Step 3: Verify.** Follow the README's quick start verbatim from `make clean` → `make smoke` passes. Every relative link resolves (`ls` each path).
- [ ] **Step 4: Stop for author review.** Commit after approval: `docs: README with quick start and local URLs`. Push → PR checks green → merge.

---

## Done When (closes slice 1)

- All six CI jobs green on the PR and on `main` after merge.
- README quick start works from `make clean`.
- Optional, author's GitHub settings: branch protection on `main` requiring these checks.
