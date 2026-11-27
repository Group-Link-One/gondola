# Leased-Trunk Flow

A git and deploy workflow for teams that share one environment.

## Summary

Leased-Trunk is trunk-based development with one addition: a branch other than trunk can take a temporary **lease** on a shared environment.

- With no lease held, every trunk build deploys to the shared environment automatically.
- While a lease is held, trunk builds stop deploying there. New builds from the branch that holds the lease keep deploying, and each one renews the lease.
- Taking a lease and ending one are both deliberate deploys, and both are recorded in the deploy log.

## The problem it solves

Git Flow, GitHub Flow and Trunk-Based Development all assume one stream of builds feeding one target. Teams that share a staging environment rarely fit that model. A typical case:

> Team B needs staging tomorrow to demo a PR. Team A keeps merging to main, and those builds are waiting to reach staging. There is no rule for who goes first.

Teams usually handle this in one of three ways:

- **Coordinate in Slack.** Easy to start, easy to drift, and the current state is visible to nobody. Every organization ends up rebuilding this informally.
- **Give every PR its own preview environment.** This is the right long-term answer, but it requires that every stateful system (databases, caches, queues, third-party integrations) can be copied per PR on demand. Most organizations cannot do that yet.
- **Let main overwrite staging anyway.** This works until it breaks someone's demo.

Leased-Trunk sits between these options. The environment stays shared, but who holds it at any moment is explicit and recorded.

## Terms

- **Trunk**: the main branch. Registered trunk builds deploy to the shared environment automatically, unless a lease blocks them.
- **Experiment branch**: any branch that is not trunk. Its first deploy to the shared environment is manual, and that deploy is what grants the lease.
- **Lease**: the state "experiment branch `X` currently holds the shared environment". There is no separate lease record. The build currently deployed to the environment defines who holds it.
- **Chunk**: a build. Not a branch or a PR, but a concrete artifact (commit SHA, image and analysis record) that can be deployed, audited and rolled back on its own.

## Rules

While branch `X` holds the lease:

| New build on | What happens |
|---|---|
| Trunk | Not deployed. The lease holder keeps the environment. |
| Branch `X` | Deployed automatically, so its author can keep iterating. |
| Any other experiment branch | Not deployed. Taking over requires a manual deploy. |

With no lease held:

| New build on | What happens |
|---|---|
| Trunk | Deployed automatically. |
| Any experiment branch | Not deployed. Moving trunk off staging has to be a manual decision. |

Going back to trunk is an ordinary trunk deploy, and it ends the lease.

## Why it works

The branching model was never the hard question. The hard question is what happens when two people want the shared staging environment at the same time, and the rest of the design follows from the answer.

Leased-Trunk ties possession to the build, not to a branch, a PR or a person. That gives four properties:

- **Reversible.** Any trunk deploy removes the experiment.
- **Auditable.** Every build that has ever held the lease appears in the deploy log.
- **Works with simple CI.** A pipeline only has to call `POST /builds`. It needs no branch policies, merge queues or per-team conventions.
- **Independent of the branching model.** The protocol lives on the deploy side. GitHub Flow, Git Flow, or the Git project's `pu`/`next`/`seen` integration branches all work underneath it.

## How it relates to other workflows

- **Trunk-Based Development.** Leased-Trunk is TBD plus an explicit handoff of the shared environment. With one producer and one deploy target, the two are identical.
- **GitHub Flow.** GitHub Flow merges to main and then deploys. Leased-Trunk does the same for trunk, and also lets a non-main build run on staging without merging, as long as that build holds the lease.
- **Git Flow and gitworkflows.** These decide how branches are integrated. Leased-Trunk decides who holds a deploy target. They solve different problems and can be used together.

## Current implementation

[Gondola](./README.md) implements Leased-Trunk for staging:

- `auto_deploy_staging: true` on a repo turns on automatic trunk deploys.
- Experiment builds (any branch other than trunk) are registered and analyzed, but by default they are not deployed automatically.
- Promoting an experiment to staging is a manual action, and it grants the lease.
- After that, new builds on the same branch deploy automatically, which renews the lease.
- Any trunk deploy ends the lease.
- `auto_deploy_experiments: true` also automates the first promotion: the first experiment build takes the lease instead of waiting for someone to promote it. Switching from one experiment to another stays manual in both modes, because moving from experiment `X` to experiment `Y` may require resetting stateful services (database, cache, queues), and the system cannot tell when that is needed.
- The reset itself can be automated. With `environments.<env>.reset.enabled`, ending a lease parks the incoming deploy, sends an `env_reset_requested` webhook, and runs the deploy only after the reset runner reports that the reset is complete. See [Environment Reset Gate](./AGENTS.md#environment-reset-gate).

The auto-deploy decision is made in `internal/worker/scheduler.go` (`shouldAutoDeploy`), and `internal/worker/autodeploy_test.go` covers the rules.

## Where it's going

Leased-Trunk is an intermediate step.

The goal is an ephemeral environment for every PR: no leases, no contention, and no question of who holds staging. That model needs automation that does not exist yet:

- **Stateful systems that can be copied per environment.** Databases (fast snapshot and restore, or realistic seed data), caches, queues, object stores and search indexes all have to be cheap to create and destroy per PR.
- **Integrations that can be replaced.** Third-party APIs and internal services that are not under test have to point at sandboxes or at deterministic fakes.
- **DNS, secrets and config generated from templates.** Each ephemeral environment needs a consistent set of endpoints and credentials, created along with it.
- **Bounded cost.** Ephemeral environments are cheap in theory and expensive in practice when nothing cleans them up.

Until that exists, standing up a new ephemeral environment costs someone an afternoon. Leased-Trunk delivers most of the coordination benefit (roughly 80%) while that infrastructure is being built.

Once it exists, the lease model is no longer needed. Each PR effectively holds a lease on its own environment, so no handoff ever has to happen.
