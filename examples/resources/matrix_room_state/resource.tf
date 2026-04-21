resource "matrix_room_state" "history_world_readable" {
  room_id      = matrix_room.example.id
  event_type   = "m.room.history_visibility"
  state_key    = ""
  content_json = jsonencode({ history_visibility = "world_readable" })
}
