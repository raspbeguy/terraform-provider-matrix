resource "matrix_room_join_rules" "general" {
  room_id   = matrix_room.general.id
  join_rule = "public"
}

# Restricted: only members of a given space can join.
resource "matrix_room_join_rules" "oncall" {
  room_id     = matrix_room.oncall.id
  join_rule   = "restricted"
  allow_rooms = [matrix_space.team.id]
}
