data "matrix_whoami" "me" {}

resource "matrix_room_power_levels" "example" {
  room_id        = matrix_room.example.id
  users_default  = 0
  events_default = 0
  state_default  = 50
  invite         = 50
  kick           = 50
  ban            = 100
  redact         = 50

  # A declared users map replaces the whole map on the homeserver, so always
  # list the account the provider runs as. Leave it out and that account drops
  # to users_default, below state_default, and loses control of the room.
  users = {
    (data.matrix_whoami.me.user_id) = 100
    "@alice:example.com"            = 100
    "@bob:example.com"              = 50
  }

  events = {
    "m.room.power_levels"       = 100
    "m.room.history_visibility" = 100
  }

  notify_room = 50
}

# Fields you leave out keep whatever the homeserver already has. This tunes one
# field of a space and leaves its other power levels untouched.
resource "matrix_room_power_levels" "unlock_space_messages" {
  room_id        = matrix_space.example.id
  events_default = 0
}
