# The escape hatch, for a state event no typed resource covers. An event type
# that a typed resource owns is refused, and the error names the resource to
# use instead.
resource "matrix_room_state" "pinned" {
  room_id    = matrix_room.example.id
  event_type = "m.room.pinned_events"
  state_key  = ""
  content_json = jsonencode({
    pinned = ["$abcdef1234567890:example.com"]
  })
}

# A custom event type with a state key.
resource "matrix_room_state" "widget" {
  room_id    = matrix_room.example.id
  event_type = "im.vector.modular.widgets"
  state_key  = "grafana"
  content_json = jsonencode({
    type = "m.custom"
    url  = "https://grafana.example.com/d/abc"
    name = "Dashboard"
  })
}
