resource "matrix_room_power_levels" "example" {
  room_id        = matrix_room.example.id
  users_default  = 0
  events_default = 0
  state_default  = 50
  invite         = 50
  kick           = 50
  ban            = 100
  redact         = 50

  users = {
    "@alice:example.com" = 100
    "@bob:example.com"   = 50
  }

  events = {
    "m.room.power_levels"       = 100
    "m.room.history_visibility" = 100
  }

  notify_room = 50
}
