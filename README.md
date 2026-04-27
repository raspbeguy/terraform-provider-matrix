# terraform-provider-matrix

[![ci](https://github.com/raspbeguy/terraform-provider-matrix/actions/workflows/ci.yml/badge.svg)](https://github.com/raspbeguy/terraform-provider-matrix/actions/workflows/ci.yml)

A Terraform / OpenTofu provider for managing [Matrix](https://matrix.org) rooms,
spaces, membership, power levels, and arbitrary state events from a user
account. Built on
[`terraform-plugin-framework`](https://github.com/hashicorp/terraform-plugin-framework)
and [`mautrix-go`](https://github.com/mautrix/go).

## Features

Resources:

| Resource | Purpose |
|---|---|
| `matrix_room` | Create and manage a room |
| `matrix_space` | Create a space (auto-applies Element's power-level defaults) |
| `matrix_space_child` | Link a room or space under a parent space |
| `matrix_room_member` | Manage one user's membership (idempotent invite/kick/ban/leave/knock) |
| `matrix_room_power_levels` | Full `m.room.power_levels` control — works on rooms and spaces |
| `matrix_room_join_rules` | `m.room.join_rules` — public/invite/knock/restricted, with space-gated `restricted` support |
| `matrix_room_server_acl` | `m.room.server_acl` — federation allow/deny lists |
| `matrix_room_alias` | Directory alias management |
| `matrix_room_state` | Arbitrary state-event escape hatch |

Data sources: `matrix_whoami`, `matrix_room` (by alias), `matrix_user`.

## Install

Available on both registries:

- HashiCorp Terraform Registry: <https://registry.terraform.io/providers/raspbeguy/matrix>
- OpenTofu Registry: <https://search.opentofu.org/provider/raspbeguy/matrix>

```hcl
terraform {
  required_providers {
    matrix = {
      source  = "raspbeguy/matrix"
      version = "~> 0.2"
    }
  }
}
```

Local development install:

```bash
make install   # builds and drops the binary under ~/.terraform.d/plugins/…
```

## Configure

All provider settings have environment-variable fallbacks.

| Attribute | Env | Required |
|---|---|---|
| `homeserver_url` | `MATRIX_HOMESERVER_URL` | yes |
| `access_token` | `MATRIX_ACCESS_TOKEN` | yes (sensitive) |
| `user_id` | `MATRIX_USER_ID` | no — inferred from `/whoami` |
| `request_timeout` | — | no — default `30s` |

Access tokens come from `/login` or, in Element, All settings → Help & About →
Advanced → Access Token.

```hcl
provider "matrix" {}   # reads MATRIX_* env vars
```

## Usage

```hcl
resource "matrix_space" "team" {
  name            = "Platform Team"
  topic           = "Umbrella space for the platform org"
  preset          = "private_chat"
  room_alias_name = "platform-team"
}

resource "matrix_room" "general" {
  name   = "#general"
  topic  = "Daily chatter"
  preset = "private_chat"
}

resource "matrix_space_child" "general" {
  parent_space_id = matrix_space.team.id
  child_room_id   = matrix_room.general.id
  suggested       = true
}

resource "matrix_room_member" "alice" {
  room_id    = matrix_room.general.id
  user_id    = "@alice:example.com"
  membership = "invite"
}
```

More examples under [`examples/`](./examples).

## Documentation

Per-resource reference lives in [`docs/`](./docs) and is published to the
Terraform / OpenTofu registry automatically on release. Regenerate locally
with:

```bash
make docs
```

## Importing existing rooms & spaces

Every resource supports `terraform import` for adopting state you already have on
the homeserver without recreating it:

```bash
terraform import matrix_room.example '!abcDEF:example.com'
terraform import matrix_space.team '!xyzGHI:example.com'
terraform import matrix_space_child.general '!xyzGHI:example.com|!abcDEF:example.com'
terraform import matrix_room_member.alice '!abcDEF:example.com|@alice:example.com'
terraform import matrix_room_power_levels.general '!abcDEF:example.com'
terraform import matrix_room_join_rules.general '!abcDEF:example.com'
terraform import matrix_room_server_acl.general '!abcDEF:example.com'
terraform import matrix_room_alias.extra '#team-general:example.com'
terraform import matrix_room_state.pins '!abcDEF:example.com|m.room.pinned_events'
```

See each resource's docs page for the exact ID format.

## Development

| Target | What it does |
|---|---|
| `make build` | Compile the provider binary |
| `make install` | Build and drop into `~/.terraform.d/plugins/…` |
| `make test` | Run unit tests |
| `make testacc` | Run acceptance tests (`TF_ACC=1`) — needs a live Matrix homeserver; see `ci/compose.synapse.yml` for a disposable one |
| `make docs` | Regenerate `docs/` via `tfplugindocs` |
| `make vet` | `go vet ./...` |

Contributions welcome. CI runs on every PR (build, vet, lint, tests, docs
drift, example formatting). A nightly acceptance workflow runs against a
containerized Synapse.

## Caveats

- Matrix doesn't let users delete rooms server-side. `terraform destroy` on a
  `matrix_room` or `matrix_space` just makes the bot leave; the room lingers.
- `matrix_room_member.reason` is a transition parameter (only attached to the
  invite/kick/ban event), not reconciled state. It's sent when the membership
  changes and not refreshed on `terraform refresh`.
- Membership transitions are idempotent: re-invoking `invite` on someone who's
  already `join` is a no-op (not a forbidden state event).
- `matrix_room_member` is declarative: if a user accepts the invite and later
  leaves the room, the next `terraform apply` will re-invite them, because the HCL
  still says `membership = "invite"`. To stop reconciling, either
  `terraform state rm` the resource (drops it from state, leaves the server
  alone) or add `lifecycle { ignore_changes = [membership] }` on the block
  (initial invite still fires, subsequent leaves are ignored).
- `matrix_space` creates rooms with Element's space power-level defaults
  (`events_default = 100`, `invite = 50`). Override via a
  `matrix_room_power_levels` resource pointing at the space.
- `matrix_room_server_acl` can irreversibly lock the caller's homeserver out
  of the room if misconfigured — once your server is blocked, you cannot send
  a corrective ACL, and only a homeserver admin can recover. The provider
  emits a plan-time warning when it detects a likely self-lockout, but
  double-check `allow` / `deny` before applying.
