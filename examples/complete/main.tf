terraform {
  required_providers {
    matrix = { source = "raspbeguy/matrix" }
  }
}

provider "matrix" {}

# End-to-end smoke: a space containing two rooms, with power levels tuned
# and one extra alias.

resource "matrix_space" "team" {
  name            = "Platform Team"
  topic           = "Umbrella space for the platform org"
  preset          = "private_chat"
  visibility      = "private"
  room_alias_name = "platform-team"
}

resource "matrix_room" "general" {
  name   = "#general"
  topic  = "Daily chatter"
  preset = "private_chat"
}

resource "matrix_room" "oncall" {
  name   = "#oncall"
  topic  = "Paging and incident coordination"
  preset = "private_chat"
}

resource "matrix_space_child" "general" {
  parent_space_id = matrix_space.team.id
  child_room_id   = matrix_room.general.id
  suggested       = true
}

resource "matrix_space_child" "oncall" {
  parent_space_id = matrix_space.team.id
  child_room_id   = matrix_room.oncall.id
  order           = "01"
}

resource "matrix_room_power_levels" "oncall" {
  room_id        = matrix_room.oncall.id
  users_default  = 0
  events_default = 50
  invite         = 50
  users = {
    (data.matrix_whoami.me.user_id) = 100
  }
}

resource "matrix_room_alias" "oncall_alt" {
  alias   = "#paging:${split(":", data.matrix_whoami.me.user_id)[1]}"
  room_id = matrix_room.oncall.id
}

data "matrix_whoami" "me" {}

output "space_id" { value = matrix_space.team.id }
output "rooms" { value = [matrix_room.general.id, matrix_room.oncall.id] }
