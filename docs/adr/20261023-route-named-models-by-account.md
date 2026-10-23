# Route named models by account

- Status: accepted
- Deciders: sullivanpj
- Date: 2026-10-23

Technical Story: Bring CLIProxyAPI's account pool, protocols, and providers into Mindctl without giving up classifier model selection.

## Context and Problem Statement

Mindctl chooses a model with a classifier when the client sends `mindctl-auto` or no model, then calls the one credential configured for that provider. CLIProxyAPI instead keeps a pool of accounts and picks one with round-robin, weights, fill-first, session stickiness, cooldown, and retry. Named-model clients need that pool. Automatic requests must keep the classifier.

## Decision Drivers

- Classifier selection stays the default whenever the request does not name a model.
- A named model must be served by the same account-selection rules CLIProxyAPI uses.
- Mindctl keeps its own gateway, history, and harness setup. CLIProxyAPI is not embedded.
- Credentials stay in local files. There is no terminal UI and no Postgres, Git, or object-storage credential store.

## Considered Options

- Reimplement account selection in Mindctl and run it only after a client names a model.
- Send named-model requests to a CLIProxyAPI process and keep automatic requests local.
- Replace the classifier with CLIProxyAPI's account router for every request.

## Decision Outcome

Chosen option: "Reimplement account selection in Mindctl and run it only after a client names a model", because the classifier remains the product difference and the gateway does not take a dependency on CLIProxyAPI. An unnamed or `mindctl-auto` request still uses the classifier, the cost policy, the model pin, and the single configured provider credential. A named model skips the classifier, keeps eligibility checks for that model, then selects an account. Unknown names are rejected. The configured credential is one account in the pool; further accounts are JSON files in the local auth directory.

### Positive Consequences

- Several subscriptions can serve one named model, with failover when an account is cooling down.
- Automatic routing, savings, and history stay on the existing path.

### Negative Consequences

- Account-pool behavior has to be maintained here as CLIProxyAPI changes.
- Named-model sessions stick to an account, which is separate from the automatic model pin and can fail over independently.
- OAuth logins, extra protocols, media, plugins, and optional Home delegation are more gateway surface than a single credential per provider.
